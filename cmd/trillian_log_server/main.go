// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The trillian_log_server binary runs the Trillian log server, and also
// provides an admin server.
package main

import (
	"context"
	"flag"
	_ "net/http/pprof" // Register pprof HTTP handlers.
	"os"
	"runtime/pprof"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/google/trillian"
	"github.com/google/trillian/cmd"
	"github.com/google/trillian/cmd/internal/serverutil"
	"github.com/google/trillian/crypto/keys/der"
	"github.com/google/trillian/crypto/keyspb"
	"github.com/google/trillian/extension"
	"github.com/google/trillian/monitoring"
	"github.com/google/trillian/monitoring/opencensus"
	"github.com/google/trillian/monitoring/prometheus"
	"github.com/google/trillian/quota"
	"github.com/google/trillian/quota/etcd"
	"github.com/google/trillian/quota/etcd/quotaapi"
	"github.com/google/trillian/quota/etcd/quotapb"
	"github.com/google/trillian/server"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/util/clock"
	etcdutil "github.com/google/trillian/util/etcd"
	"google.golang.org/grpc"

	// Register key ProtoHandlers
	_ "github.com/google/trillian/crypto/keys/der/proto"
	_ "github.com/google/trillian/crypto/keys/pem/proto"
	_ "github.com/google/trillian/crypto/keys/pkcs11/proto"

	// Register supported storage providers.
	_ "github.com/google/trillian/storage/cloudspanner"
	_ "github.com/google/trillian/storage/mysql"

	// Load hashers
	_ "github.com/google/trillian/merkle/rfc6962"

	// Load MySQL quota provider
	_ "github.com/google/trillian/quota/mysqlqm"
)

var (
	rpcEndpoint     = flag.String("rpc_endpoint", "localhost:8090", "Endpoint for RPC requests (host:port)")
	httpEndpoint    = flag.String("http_endpoint", "localhost:8091", "Endpoint for HTTP metrics (host:port, empty means disabled)")
	healthzTimeout  = flag.Duration("healthz_timeout", time.Second*5, "Timeout used during healthz checks")
	tlsCertFile     = flag.String("tls_cert_file", "", "Path to the TLS server certificate. If unset, the server will use unsecured connections.")
	tlsKeyFile      = flag.String("tls_key_file", "", "Path to the TLS server key. If unset, the server will use unsecured connections.")
	etcdService     = flag.String("etcd_service", "trillian-logserver", "Service name to announce ourselves under")
	etcdHTTPService = flag.String("etcd_http_service", "trillian-logserver-http", "Service name to announce our HTTP endpoint under")

	quotaDryRun = flag.Bool("quota_dry_run", false, "If true no requests are blocked due to lack of tokens")

	treeGCEnabled            = flag.Bool("tree_gc", true, "If true, tree garbage collection (hard-deletion) is periodically performed")
	treeDeleteThreshold      = flag.Duration("tree_delete_threshold", serverutil.DefaultTreeDeleteThreshold, "Minimum period a tree has to remain deleted before being hard-deleted")
	treeDeleteMinRunInterval = flag.Duration("tree_delete_min_run_interval", serverutil.DefaultTreeDeleteMinInterval, "Minimum interval between tree garbage collection sweeps. Actual runs happen randomly between [minInterval,2*minInterval).")

	tracing          = flag.Bool("tracing", false, "If true opencensus Stackdriver tracing will be enabled. See https://opencensus.io/.")
	tracingProjectID = flag.String("tracing_project_id", "", "project ID to pass to stackdriver. Can be empty for GCP, consult docs for other platforms.")
	tracingPercent   = flag.Int("tracing_percent", 0, "Percent of requests to be traced. Zero is a special case to use the DefaultSampler")

	configFile            = flag.String("config", "", "Config file containing flags, file contents can be overridden by command line flags")
	maxReceiveMessageSize = flag.Int("max_receive_message_size", 0, "Set the maximum receive message size for the log server")

	// Profiling related flags.
	cpuProfile = flag.String("cpuprofile", "", "If set, write CPU profile to this file")
	memProfile = flag.String("memprofile", "", "If set, write memory profile to this file")
)

func main() {
	flag.Parse()
	defer glog.Flush()

	if *configFile != "" {
		if err := cmd.ParseFlagFile(*configFile); err != nil {
			glog.Exitf("Failed to load flags from config file %q: %s", *configFile, err)
		}
	}

	ctx := context.Background()

	var options []grpc.ServerOption
	mf := prometheus.MetricFactory{}
	monitoring.SetStartSpan(opencensus.StartSpan)

	if *tracing {
		opts, err := opencensus.EnableRPCServerTracing(*tracingProjectID, *tracingPercent)
		if err != nil {
			glog.Exitf("Failed to initialize stackdriver / opencensus tracing: %v", err)
		}
		// Enable the server request counter tracing etc.
		options = append(options, opts...)
	}

	// increase max receive msg size to allow listing of thousands of trees
	if *maxReceiveMessageSize != 0 {
		options = append(options, grpc.MaxRecvMsgSize(*maxReceiveMessageSize))
		glog.Infof("Received max msg size option: %d", *maxReceiveMessageSize)
	}

	sp, err := storage.NewProviderFromFlags(mf)
	if err != nil {
		glog.Exitf("Failed to get storage provider: %v", err)
	}
	defer sp.Close()

	client, err := etcdutil.NewClientFromString(*etcd.Servers)
	if err != nil {
		glog.Exitf("Failed to connect to etcd at %v: %v", *etcd.Servers, err)
	}

	// Announce our endpoints to etcd if so configured.
	unannounce := serverutil.AnnounceSelf(ctx, client, *etcdService, *rpcEndpoint)
	defer unannounce()
	if *httpEndpoint != "" {
		unannounceHTTP := serverutil.AnnounceSelf(ctx, client, *etcdHTTPService, *httpEndpoint)
		defer unannounceHTTP()
	}

	qm, err := quota.NewManagerFromFlags()
	if err != nil {
		glog.Exitf("Error creating quota manager: %v", err)
	}

	registry := extension.Registry{
		AdminStorage:  sp.AdminStorage(),
		LogStorage:    sp.LogStorage(),
		QuotaManager:  qm,
		MetricFactory: mf,
		NewKeyProto: func(ctx context.Context, spec *keyspb.Specification) (proto.Message, error) {
			return der.NewProtoFromSpec(spec)
		},
	}

	// Enable CPU profile if requested.
	if *cpuProfile != "" {
		f := mustCreate(*cpuProfile)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	m := serverutil.Main{
		RPCEndpoint:  *rpcEndpoint,
		HTTPEndpoint: *httpEndpoint,
		TLSCertFile:  *tlsCertFile,
		TLSKeyFile:   *tlsKeyFile,
		StatsPrefix:  "log",
		ExtraOptions: options,
		QuotaDryRun:  *quotaDryRun,
		DBClose:      sp.Close,
		Registry:     registry,
		RegisterServerFn: func(s *grpc.Server, registry extension.Registry) error {
			logServer := server.NewTrillianLogRPCServer(registry, clock.System)
			if err := logServer.IsHealthy(); err != nil {
				return err
			}
			trillian.RegisterTrillianLogServer(s, logServer)
			if *quota.System == etcd.QuotaManagerName {
				quotapb.RegisterQuotaServer(s, quotaapi.NewServer(client))
			}
			return nil
		},
		IsHealthy: func(ctx context.Context) error {
			as := sp.AdminStorage()
			return as.CheckDatabaseAccessible(ctx)
		},
		HealthyDeadline:       *healthzTimeout,
		AllowedTreeTypes:      []trillian.TreeType{trillian.TreeType_LOG, trillian.TreeType_PREORDERED_LOG},
		TreeGCEnabled:         *treeGCEnabled,
		TreeDeleteThreshold:   *treeDeleteThreshold,
		TreeDeleteMinInterval: *treeDeleteMinRunInterval,
	}

	if err := m.Run(ctx); err != nil {
		glog.Exitf("Server exited with error: %v", err)
	}

	if *memProfile != "" {
		f := mustCreate(*memProfile)
		pprof.WriteHeapProfile(f)
	}
}

func mustCreate(fileName string) *os.File {
	f, err := os.Create(fileName)
	if err != nil {
		glog.Fatal(err)
	}
	return f
}
