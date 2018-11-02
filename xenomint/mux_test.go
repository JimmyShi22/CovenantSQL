/*
 * Copyright 2018 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package xenomint

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/CovenantSQL/CovenantSQL/conf"
	con "github.com/CovenantSQL/CovenantSQL/consistent"
	ca "github.com/CovenantSQL/CovenantSQL/crypto/asymmetric"
	"github.com/CovenantSQL/CovenantSQL/crypto/kms"
	"github.com/CovenantSQL/CovenantSQL/proto"
	"github.com/CovenantSQL/CovenantSQL/route"
	"github.com/CovenantSQL/CovenantSQL/rpc"
	wt "github.com/CovenantSQL/CovenantSQL/worker/types"
)

const (
	benchmarkRPCName    = "BENCH"
	benchmarkDatabaseID = "0x0"
)

type localServer struct {
	node   proto.Node
	server *rpc.Server
}

func setupBenchmarkGlobal(b *testing.B) (
	lbp, ls *localServer, ms *MuxService, n int, r []*MuxQueryRequest,
) {
	// Create 3 node IDs: 1 BP, 1 Miner, and 1 Client
	const (
		vnum    = 3
		vlen    = 100
		records = 1000
	)
	var (
		dht   *route.DHTService
		nis   []proto.Node
		err   error
		priv  *ca.PrivateKey
		bp, s *rpc.Server
	)
	if priv, _, err = ca.GenSecp256k1KeyPair(); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}
	if nis, err = mineNoncesFromPublicKey(priv.PubKey(), testingNonceDifficulty, 3); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	} else if l := len(nis); l != 3 {
		b.Fatalf("Failed to setup bench environment: unexpected length %d", l)
	}
	// Create BP and local RPC servers and update server address
	bp = rpc.NewServer()
	if err = bp.InitRPCServer("localhost:0", testingPrivateKeyFile, testingMasterKey); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}
	nis[0].Addr = bp.Listener.Addr().String()
	nis[0].Role = proto.Leader
	go bp.Serve()
	s = rpc.NewServer()
	if err = s.InitRPCServer("localhost:0", testingPrivateKeyFile, testingMasterKey); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}
	nis[1].Addr = s.Listener.Addr().String()
	nis[1].Role = proto.Miner
	go s.Serve()
	// Create client
	nis[2].Role = proto.Client
	// Setup global config
	conf.GConf = &conf.Config{
		IsTestMode:          true,
		GenerateKeyPair:     false,
		MinNodeIDDifficulty: testingNonceDifficulty,
		BP: &conf.BPInfo{
			PublicKey: priv.PubKey(),
			NodeID:    nis[0].ID,
			Nonce:     nis[0].Nonce,
		},
		KnownNodes: nis,
	}
	// Register DHT service
	if dht, err = route.NewDHTService(testingDHTDBFile, &con.KMSStorage{}, true); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	} else if err = bp.RegisterService(route.DHTRPCName, dht); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}
	kms.SetLocalNodeIDNonce(nis[2].ID.ToRawNodeID().CloneBytes(), &nis[2].Nonce)
	for i := range nis {
		route.SetNodeAddrCache(nis[i].ID.ToRawNodeID(), nis[i].Addr)
		kms.SetNode(&nis[i])
	}
	// Register mux service
	if ms, err = NewMuxService(benchmarkRPCName, s); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}

	// Setup query requests
	var (
		sel = `SELECT "v1", "v2", "v3" FROM "bench" WHERE "k"=?`
		ins = `INSERT INTO "bench" VALUES (?, ?, ?, ?)
	ON CONFLICT("k") DO UPDATE SET
		"v1"="excluded"."v1",
		"v2"="excluded"."v2",
		"v3"="excluded"."v3"
`
		src = make([][]interface{}, records)
	)
	n = records
	r = make([]*MuxQueryRequest, 2*records)
	// Read query key space [0, n-1]
	for i := 0; i < records; i++ {
		var req = buildRequest(wt.ReadQuery, []wt.Query{
			buildQuery(sel, i),
		})
		if err = req.Sign(priv); err != nil {
			b.Fatalf("Failed to setup bench environment: %v", err)
		}
		r[i] = &MuxQueryRequest{
			DatabaseID: benchmarkDatabaseID,
			Request:    req,
		}
	}
	// Write query key space [n, 2n-1]
	for i := range src {
		var vals [vnum][vlen]byte
		src[i] = make([]interface{}, vnum+1)
		src[i][0] = i + records
		for j := range vals {
			rand.Read(vals[j][:])
			src[i][j+1] = string(vals[j][:])
		}
	}
	for i := 0; i < records; i++ {
		var req = buildRequest(wt.WriteQuery, []wt.Query{
			buildQuery(ins, src[i]...),
		})
		if err = req.Sign(priv); err != nil {
			b.Fatalf("Failed to setup bench environment: %v", err)
		}
		r[records+i] = &MuxQueryRequest{
			DatabaseID: benchmarkDatabaseID,
			Request:    req,
		}
	}

	lbp = &localServer{
		node:   nis[0],
		server: bp,
	}
	ls = &localServer{
		node:   nis[1],
		server: s,
	}

	return
}

func teardownBenchmarkGlobal(b *testing.B, bp, s *rpc.Server) {
	s.Stop()
	bp.Stop()
}

func setupBenchmarkMux(b *testing.B, ms *MuxService) (c *Chain) {
	const (
		vnum    = 3
		vlen    = 100
		records = 1000
	)
	// Setup chain state
	var (
		fl   = path.Join(testingDataDir, strings.Replace(b.Name(), "/", "-", -1))
		err  error
		stmt *sql.Stmt
	)
	if c, err = NewChain(fmt.Sprint("file:", fl)); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}
	if _, err = c.state.strg.Writer().Exec(
		`CREATE TABLE "bench" ("k" INT, "v1" TEXT, "v2" TEXT, "v3" TEXT, PRIMARY KEY("k"))`,
	); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}
	if stmt, err = c.state.strg.Writer().Prepare(
		`INSERT INTO "bench" VALUES (?, ?, ?, ?)`,
	); err != nil {
		b.Fatalf("Failed to setup bench environment: %v", err)
	}
	for i := 0; i < records; i++ {
		var (
			vals [vnum][vlen]byte
			args [vnum + 1]interface{}
		)
		args[0] = i
		for i := range vals {
			rand.Read(vals[i][:])
			args[i+1] = string(vals[i][:])
		}
		if _, err = stmt.Exec(args[:]...); err != nil {
			b.Fatalf("Failed to setup bench environment: %v", err)
		}
	}
	ms.register(benchmarkDatabaseID, c)

	b.ResetTimer()
	return
}

func teardownBenchmarkMux(b *testing.B, ms *MuxService) {
	b.StopTimer()

	var (
		fl  = path.Join(testingDataDir, strings.Replace(b.Name(), "/", "-", -1))
		err error
		c   *Chain
	)
	// Stop RPC server
	if c, err = ms.route(benchmarkDatabaseID); err != nil {
		b.Fatalf("Failed to teardown bench environment: %v", err)
	}
	ms.unregister(benchmarkDatabaseID)
	// Close chain
	if err = c.close(); err != nil {
		b.Fatalf("Failed to teardown bench environment: %v", err)
	}
	if err = os.Remove(fl); err != nil {
		b.Fatalf("Failed to teardown bench environment: %v", err)
	}
	if err = os.Remove(fmt.Sprint(fl, "-shm")); err != nil && !os.IsNotExist(err) {
		b.Fatalf("Failed to teardown bench environment: %v", err)
	}
	if err = os.Remove(fmt.Sprint(fl, "-wal")); err != nil && !os.IsNotExist(err) {
		b.Fatalf("Failed to teardown bench environment: %v", err)
	}
}

func BenchmarkMuxParallel(b *testing.B) {
	var (
		bp, s, ms, n, r = setupBenchmarkGlobal(b)
		benchmarks      = []struct {
			name    string
			randfun func(n int) int
		}{
			{
				name: "Write",
				randfun: func(n int) int {
					return n + rand.Intn(n)
				},
			},
			{
				name: "MixRW",
				randfun: func(n int) int {
					return rand.Intn(2 * n)
				},
			},
		}
	)
	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			var c = setupBenchmarkMux(b, ms)
			defer teardownBenchmarkMux(b, ms)
			b.RunParallel(func(pb *testing.PB) {
				var (
					err    error
					method = fmt.Sprintf("%s.%s", benchmarkRPCName, "Query")
					cl     = rpc.NewPersistentCaller(s.node.ID)
				)
				for i := 0; pb.Next(); i++ {
					if err = cl.Call(method, &r[bm.randfun(n)], &MuxQueryResponse{}); err != nil {
						b.Fatalf("Failed to execute: %v", err)
					}
					if (i+1)%benchmarkQueriesPerBlock == 0 {
						if _, err = c.state.commit(); err != nil {
							b.Fatalf("Failed to commit block: %v", err)
						}
					}
				}
			})
		})
	}
	teardownBenchmarkGlobal(b, bp.server, s.server)
}