// Copyright 2022 PingCAP, Inc.
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

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/pingcap/TiProxy/lib/config"
	"github.com/pingcap/TiProxy/lib/util/errors"
	"github.com/pingcap/TiProxy/lib/util/security"
	"github.com/pingcap/TiProxy/lib/util/waitgroup"
	mgrcfg "github.com/pingcap/TiProxy/pkg/manager/config"
	"github.com/pingcap/TiProxy/pkg/manager/logger"
	mgrns "github.com/pingcap/TiProxy/pkg/manager/namespace"
	"github.com/pingcap/TiProxy/pkg/manager/router"
	"github.com/pingcap/TiProxy/pkg/metrics"
	"github.com/pingcap/TiProxy/pkg/proxy"
	"github.com/pingcap/TiProxy/pkg/server/api"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type Server struct {
	// managers
	ConfigManager    *mgrcfg.ConfigManager
	NamespaceManager *mgrns.NamespaceManager
	MetricsManager   *metrics.MetricsManager
	LoggerManager    *logger.LoggerManager
	ObserverClient   *clientv3.Client
	// HTTP client
	Http *http.Client
	// HTTP/GRPC services
	Etcd *embed.Etcd
	// L7 proxy
	Proxy *proxy.SQLServer
}

func NewServer(ctx context.Context, cfg *config.Config, pubAddr string) (srv *Server, err error) {
	srv = &Server{
		ConfigManager:    mgrcfg.NewConfigManager(),
		MetricsManager:   metrics.NewMetricsManager(),
		NamespaceManager: mgrns.NewNamespaceManager(),
	}

	// set up logger
	var lg *zap.Logger
	if srv.LoggerManager, lg, err = logger.NewLoggerManager(&cfg.Log); err != nil {
		return
	}

	{
		tlogger := lg.Named("tls")
		// auto generate CA for serverTLS will break
		if uerr := security.AutoTLS(tlogger, &cfg.Security.ServerTLS, false, cfg.Workdir, "server", cfg.Security.RSAKeySize); uerr != nil {
			err = errors.WithStack(uerr)
			return
		}
		if uerr := security.AutoTLS(tlogger, &cfg.Security.PeerTLS, true, cfg.Workdir, "peer", cfg.Security.RSAKeySize); uerr != nil {
			err = errors.WithStack(uerr)
			return
		}
	}

	ready := atomic.NewBool(false)

	// setup metrics
	srv.MetricsManager.Init(ctx, lg.Named("metrics"), cfg.Metrics.MetricsAddr, cfg.Metrics.MetricsInterval, cfg.Proxy.Addr)

	// setup gin and etcd
	{
		gin.SetMode(gin.ReleaseMode)
		engine := gin.New()
		engine.Use(
			gin.Recovery(),
			ginzap.Ginzap(lg.Named("gin"), "", true),
			func(c *gin.Context) {
				if !ready.Load() {
					c.Abort()
					c.JSON(http.StatusInternalServerError, "service not ready")
				}
			},
		)

		// This is the tricky part. While HTTP services rely on managers, managers also rely on the etcd server.
		// Etcd server is used to bring up the config manager and HTTP services itself.
		// That means we have cyclic dependencies. Here's the solution:
		// 1. create managers first, and pass them down
		// 2. start etcd and HTTP, but HTTP will wait for managers to init
		// 3. init managers using bootstrapped etcd
		//
		// We have some alternative solution, for example:
		// 1. globally lazily creation of managers. It introduced racing/chaos-management/hard-code-reading as in TiDB.
		// 2. pass down '*Server' struct such that the underlying relies on the pointer only. But it does not work well for golang. To avoid cyclic imports between 'api' and `server` packages, two packages needs to be merged. That is basically what happened to TiDB '*Session'.
		api.Register(engine.Group("/api"), cfg.API, lg.Named("api"), srv.NamespaceManager, srv.ConfigManager)

		srv.Etcd, err = buildEtcd(cfg, lg.Named("etcd"), pubAddr, engine)
		if err != nil {
			err = errors.WithStack(err)
			return
		}

		// wait for etcd server
		select {
		case <-ctx.Done():
			err = fmt.Errorf("timeout on creating etcd")
			return
		case <-srv.Etcd.Server.ReadyNotify():
		}
	}

	// general cluster HTTP client
	{
		clientTLS, uerr := security.BuildClientTLSConfig(lg.Named("http"), cfg.Security.ClusterTLS)
		if uerr != nil {
			err = errors.WithStack(err)
			return
		}
		srv.Http = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: clientTLS,
			},
		}
	}

	// setup config manager
	{
		err = srv.ConfigManager.Init(ctx, srv.Etcd.Server.KV(), cfg, lg.Named("config"))
		if err != nil {
			err = errors.WithStack(err)
			return
		}
		srv.LoggerManager.Init(srv.ConfigManager.GetLogConfigWatch())

		nscs, nerr := srv.ConfigManager.ListAllNamespace(ctx)
		if nerr != nil {
			err = errors.WithStack(nerr)
			return
		}
		if len(nscs) == 0 {
			// no existed namespace
			nsc := &config.Namespace{
				Namespace: "default",
				Backend: config.BackendNamespace{
					Instances:    []string{},
					SelectorType: "random",
				},
			}
			if err = srv.ConfigManager.SetNamespace(ctx, nsc.Namespace, nsc); err != nil {
				return
			}
		}
	}

	// setup namespace manager
	{
		srv.ObserverClient, err = router.InitEtcdClient(lg.Named("pd"), cfg)
		if err != nil {
			err = errors.WithStack(err)
			return
		}

		var nss []*config.Namespace
		nss, err = srv.ConfigManager.ListAllNamespace(ctx)
		if err != nil {
			err = errors.WithStack(err)
			return
		}

		err = srv.NamespaceManager.Init(lg.Named("nsmgr"), nss, srv.ObserverClient, srv.Http)
		if err != nil {
			err = errors.WithStack(err)
			return
		}
	}

	// setup proxy server
	{
		srv.Proxy, err = proxy.NewSQLServer(lg.Named("proxy"), cfg.Proxy, cfg.Security, srv.NamespaceManager)
		if err != nil {
			err = errors.WithStack(err)
			return
		}
	}

	ready.Toggle()
	return
}

func (s *Server) Run(ctx context.Context) {
	s.Proxy.Run(ctx, s.ConfigManager.GetProxyConfigWatch())
}

func (s *Server) Close() error {
	errs := make([]error, 0, 4)
	if s.Proxy != nil {
		errs = append(errs, s.Proxy.Close())
	}
	if s.NamespaceManager != nil {
		errs = append(errs, s.NamespaceManager.Close())
	}
	if s.ConfigManager != nil {
		errs = append(errs, s.ConfigManager.Close())
	}
	if s.ObserverClient != nil {
		errs = append(errs, s.ObserverClient.Close())
	}
	if s.Etcd != nil {
		var wg waitgroup.WaitGroup
		wg.Run(func() {
			for {
				err, ok := <-s.Etcd.Err()
				if !ok {
					break
				}
				errs = append(errs, err)
			}
		})
		s.Etcd.Close()
		wg.Wait()
	}
	if s.MetricsManager != nil {
		s.MetricsManager.Close()
	}
	if s.LoggerManager != nil {
		if err := s.LoggerManager.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Collect(ErrCloseServer, errs...)
}

func buildEtcd(cfg *config.Config, logger *zap.Logger, pubAddr string, engine *gin.Engine) (srv *embed.Etcd, err error) {
	etcd_cfg := embed.NewConfig()

	if etcd_cfg.ClientTLSInfo, etcd_cfg.PeerTLSInfo, err = security.BuildEtcdTLSConfig(logger, cfg.Security.ServerTLS, cfg.Security.PeerTLS); err != nil {
		return
	}

	apiAddr, err := url.Parse(fmt.Sprintf("http://%s", cfg.API.Addr))
	if err != nil {
		return nil, err
	}
	if etcd_cfg.ClientTLSInfo.Empty() {
		apiAddr.Scheme = "http"
	} else {
		apiAddr.Scheme = "https"
	}
	etcd_cfg.LCUrls = []url.URL{*apiAddr}
	apiAddrAdvertise := *apiAddr
	apiAddrAdvertise.Host = fmt.Sprintf("%s:%s", pubAddr, apiAddrAdvertise.Port())
	etcd_cfg.ACUrls = []url.URL{apiAddrAdvertise}

	peerPort := cfg.Advance.PeerPort
	if peerPort == "" {
		peerPortNum, err := strconv.Atoi(apiAddr.Port())
		if err != nil {
			return nil, err
		}
		peerPort = strconv.Itoa(peerPortNum + 1)
	}
	peerAddr := *apiAddr
	if etcd_cfg.PeerTLSInfo.Empty() {
		peerAddr.Scheme = "http"
	} else {
		peerAddr.Scheme = "https"
	}
	peerAddr.Host = fmt.Sprintf("%s:%s", peerAddr.Hostname(), peerPort)
	etcd_cfg.LPUrls = []url.URL{peerAddr}
	peerAddrAdvertise := peerAddr
	peerAddrAdvertise.Host = fmt.Sprintf("%s:%s", pubAddr, peerPort)
	etcd_cfg.APUrls = []url.URL{peerAddrAdvertise}

	etcd_cfg.Name = "proxy-" + fmt.Sprint(time.Now().UnixMicro())
	etcd_cfg.InitialCluster = etcd_cfg.InitialClusterFromName(etcd_cfg.Name)
	etcd_cfg.Dir = filepath.Join(cfg.Workdir, "etcd")
	etcd_cfg.ZapLoggerBuilder = embed.NewZapLoggerBuilder(logger)

	etcd_cfg.UserHandlers = map[string]http.Handler{
		"/api/": engine,
	}
	return embed.StartEtcd(etcd_cfg)
}
