// Copyright 2017 The Cockroach Authors.
//
// Licensed under the Cockroach Community Licence (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/LICENSE

package acceptanceccl

// The benchmarks in this file are remote tests that use Terrafarm to manage
// and run tests against dedicated test clusters. See allocator_test.go for
// instructions on how to set this up to run locally. Also note that you likely
// want to increase `-benchtime` to something more like 5m (the default is 1s).

import (
	"bytes"
	gosql "database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/acceptance"
	"github.com/cockroachdb/cockroach/pkg/acceptance/terrafarm"
	"github.com/cockroachdb/cockroach/pkg/ccl/sqlccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

const (
	longWaitTime        = 2 * time.Minute
	bulkArchiveStoreURL = "gs://cockroach-test/bulkops/10nodes-2t-50000ranges"
)

type benchmarkTest struct {
	b testing.TB
	// nodes is the number of nodes this cluster will have.
	nodes int
	// prefix is the prefix that will be prepended to all resources created by
	// Terraform.
	prefix string
	// cockroachDiskSizeGB is the size, in gigabytes, of the disks allocated
	// for CockroachDB nodes. Leaving this as 0 accepts the default in the
	// Terraform configs. This must be in GB, because Terraform only accepts
	// disk size for GCE in GB.
	cockroachDiskSizeGB int
	// storeURL is the Google Cloud Storage URL from which the test will
	// download stores. Nothing is downloaded if storeURL is empty.
	storeURL string
	// skipClusterInit controls the --join flags for the nodes. If false (the
	// default), then the first node will be empty and thus init the cluster,
	// and each node will have the previous node as its join flag. If true,
	// then all nodes will have all nodes in their join flags.
	skipClusterInit bool

	f *terrafarm.Farmer
}

func (bt *benchmarkTest) Start(ctx context.Context) {
	bt.f = acceptance.MakeFarmer(bt.b, bt.prefix, acceptance.GetStopper())

	bt.f.AddFlag("--max-offset=1s")

	bt.f.AddVars["join_all"] = fmt.Sprint(bt.skipClusterInit)

	if bt.cockroachDiskSizeGB != 0 {
		bt.f.AddVars["cockroach_disk_size"] = strconv.Itoa(bt.cockroachDiskSizeGB)
	}

	log.Infof(ctx, "creating cluster with %d node(s)", bt.nodes)
	if err := bt.f.Resize(bt.nodes); err != nil {
		bt.b.Fatal(err)
	}

	if bt.storeURL != "" {
		// We must stop the cluster because `nodectl` pokes at the data directory.
		log.Info(ctx, "stopping cluster")
		for i := 0; i < bt.f.NumNodes(); i++ {
			if err := bt.f.Kill(ctx, i); err != nil {
				bt.b.Fatalf("error stopping node %d: %s", i, err)
			}
		}

		log.Info(ctx, "downloading archived stores from Google Cloud Storage in parallel")
		errors := make(chan error, bt.f.NumNodes())
		for i := 0; i < bt.f.NumNodes(); i++ {
			go func(nodeNum int) {
				cmd := fmt.Sprintf(`gsutil -m cp -r "%s/node%d/*" "%s"`, bt.storeURL, nodeNum, "/mnt/data0")
				log.Infof(ctx, "exec on node %d: %s", nodeNum, cmd)
				errors <- bt.f.Exec(nodeNum, cmd)
			}(i)
		}
		for i := 0; i < bt.f.NumNodes(); i++ {
			if err := <-errors; err != nil {
				bt.b.Fatalf("error downloading store %d: %s", i, err)
			}
		}

		log.Info(ctx, "restarting cluster with archived store(s)")
		for i := 0; i < bt.f.NumNodes(); i++ {
			if err := bt.f.Restart(ctx, i); err != nil {
				bt.b.Fatalf("error restarting node %d: %s", i, err)
			}
		}
	}
	acceptance.CheckGossip(ctx, bt.b, bt.f, longWaitTime, acceptance.HasPeers(bt.nodes))
	bt.f.Assert(ctx, bt.b)

	sqlDB, err := gosql.Open("postgres", bt.f.PGUrl(ctx, 0))
	if err != nil {
		bt.b.Fatal(err)
	}
	defer sqlDB.Close()
	sqlutils.MakeSQLRunner(bt.b, sqlDB).Exec("SET CLUSTER SETTING enterprise.enabled = true")

	log.Info(ctx, "initial cluster is up")
}

func (bt *benchmarkTest) Close(ctx context.Context) {
	if r := recover(); r != nil {
		bt.b.Errorf("recovered from panic to destroy cluster: %v", r)
	}
	if bt.f != nil {
		log.Infof(ctx, "shutting down cluster")
		bt.f.MustDestroy(bt.b)
	}
}

const (
	backupRestoreRowPayloadSize = 100

	// TODO(mjibson): attempt to unify these with the identical ones in sqlccl.
	bankCreateDatabase = `CREATE DATABASE bench`
	bankCreateTable    = `CREATE TABLE bench.bank (
		id INT PRIMARY KEY,
		balance INT,
		payload STRING,
		FAMILY (id, balance, payload)
	)`
	bankInsert = `INSERT INTO bench.bank VALUES (%d, %d, '%s')`
)

func getAzureURI(t testing.TB) url.URL {
	container := os.Getenv("AZURE_CONTAINER")
	accountName := os.Getenv("AZURE_ACCOUNT_NAME")
	accountKey := os.Getenv("AZURE_ACCOUNT_KEY")
	if container == "" || accountName == "" || accountKey == "" {
		t.Fatal("env variables AZURE_CONTAINER, AZURE_ACCOUNT_NAME, AZURE_ACCOUNT_KEY must be set")
	}

	return url.URL{
		Scheme: "azure",
		Host:   container,
		RawQuery: url.Values{
			storageccl.AzureAccountNameParam: []string{accountName},
			storageccl.AzureAccountKeyParam:  []string{accountKey},
		}.Encode(),
	}
}

// BenchmarkRestoreBig creates a backup via Load with b.N rows then benchmarks
// the time to restore it. Run with:
// make bench TESTTIMEOUT=1h PKG=./pkg/ccl/acceptanceccl BENCHES=BenchmarkRestoreBig TESTFLAGS='-v -benchtime 1m -remote -key-name azure -cwd ../../acceptance/terraform/azure'
func BenchmarkRestoreBig(b *testing.B) {
	ctx := context.Background()
	rng, _ := randutil.NewPseudoRand()

	restoreBaseURI := getAzureURI(b)

	bt := benchmarkTest{
		b:                   b,
		nodes:               3,
		cockroachDiskSizeGB: 250,
		prefix:              "restore",
	}

	defer bt.Close(ctx)
	bt.Start(ctx)

	sqlDB, err := gosql.Open("postgres", bt.f.PGUrl(ctx, 0))
	if err != nil {
		b.Fatal(err)
	}
	defer sqlDB.Close()

	r := sqlutils.MakeSQLRunner(b, sqlDB)

	r.Exec(bankCreateDatabase)

	// (mis-)Use a sub benchmark to avoid running the setup code more than once.
	b.Run("", func(b *testing.B) {
		restoreBaseURI.Path = fmt.Sprintf("BenchmarkRestoreBig/%s-%d", timeutil.Now().Format(time.RFC3339Nano), b.N)

		var buf bytes.Buffer
		buf.WriteString(bankCreateTable)
		buf.WriteString(";\n")
		for i := 0; i < b.N; i++ {
			payload := randutil.RandBytes(rng, backupRestoreRowPayloadSize)
			fmt.Fprintf(&buf, bankInsert, i, 0, payload)
			buf.WriteString(";\n")
		}

		ts := hlc.Timestamp{WallTime: hlc.UnixNano()}
		restoreURI := restoreBaseURI.String()
		desc, err := sqlccl.Load(ctx, sqlDB, &buf, "bench", restoreURI, ts, 0, os.TempDir())
		if err != nil {
			b.Fatal(err)
		}

		dbName := fmt.Sprintf("bank%d", b.N)
		r.Exec(fmt.Sprintf("CREATE DATABASE %s", dbName))

		b.ResetTimer()
		log.Infof(ctx, "starting restore to %s", dbName)
		r.Exec(fmt.Sprintf(`RESTORE TABLE bench.* FROM $1 WITH OPTIONS ('into_db'='%s')`, dbName), restoreURI)
		b.SetBytes(desc.DataSize / int64(b.N))
		log.Infof(ctx, "restored %s", humanizeutil.IBytes(desc.DataSize))
		b.StopTimer()
	})
}

func BenchmarkRestore2TB(b *testing.B) {
	if b.N != 1 {
		b.Fatal("b.N must be 1")
	}

	const backupBaseURI = "gs://cockroach-test/2t-backup"

	bt := benchmarkTest{
		b: b,
		// TODO(dan): This is intended to be a 10 node test, but gce local ssds
		// are only available as 375GB, which doesn't fit a 2TB restore (at
		// least until #15210 is fixed). We could have more than one ssd per
		// machine and raid them together but in the lead up to 1.0, I'm trying
		// to change as little as possible while getting this working. Azure has
		// large storage machines available, but has other issues we're working
		// through (#15381).
		nodes:               15,
		cockroachDiskSizeGB: 250,
		prefix:              "restore2tb",
	}

	ctx := context.Background()
	bt.Start(ctx)
	defer bt.Close(ctx)

	db, err := gosql.Open("postgres", bt.f.PGUrl(ctx, 0))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec("CREATE DATABASE datablocks"); err != nil {
		b.Fatal(err)
	}

	if _, err := db.Exec(`RESTORE datablocks.* FROM $1`, backupBaseURI); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkBackup2TB(b *testing.B) {
	if b.N != 1 {
		b.Fatal("b.N must be 1")
	}

	backupBaseURI := getAzureURI(b)

	backupBaseURI = url.URL{
		Scheme: "gs",
		Host:   "cockroach-test",
	}

	bt := benchmarkTest{
		b:                   b,
		nodes:               10,
		storeURL:            bulkArchiveStoreURL,
		cockroachDiskSizeGB: 250,
		prefix:              "backup2tb",
		skipClusterInit:     true,
	}

	ctx := context.Background()
	bt.Start(ctx)
	defer bt.Close(ctx)

	db, err := gosql.Open("postgres", bt.f.PGUrl(ctx, 0))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	backupBaseURI.Path = fmt.Sprintf("BenchmarkBackup2TB/%s-%d", timeutil.Now().Format(time.RFC3339Nano), b.N)

	log.Infof(ctx, "starting backup")
	row := db.QueryRow(`BACKUP DATABASE datablocks TO $1`, backupBaseURI.String())
	var unused string
	var dataSize int64
	if err := row.Scan(&unused, &unused, &unused, &dataSize); err != nil {
		bt.b.Fatal(err)
	}
	b.SetBytes(dataSize)
	log.Infof(ctx, "backed up %s", humanizeutil.IBytes(dataSize))
}
