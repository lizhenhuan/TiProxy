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

package backend

import (
	"crypto/tls"
	"testing"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/pingcap/TiProxy/lib/util/logger"
	pnet "github.com/pingcap/TiProxy/pkg/proxy/net"
	"go.uber.org/zap"
)

type proxyConfig struct {
	frontendTLSConfig *tls.Config
	backendTLSConfig  *tls.Config
	sessionToken      string
	capability        uint32
	waitRedirect      bool
}

func newProxyConfig() *proxyConfig {
	return &proxyConfig{
		capability:   defaultTestBackendCapability,
		sessionToken: mockToken,
	}
}

type mockProxy struct {
	*proxyConfig
	*BackendConnManager
	// outputs that received from the server.
	rs *gomysql.Result
	// execution results
	err         error
	logger      *zap.Logger
	holdRequest bool
}

func newMockProxy(t *testing.T, cfg *proxyConfig) *mockProxy {
	mp := &mockProxy{
		proxyConfig:        cfg,
		logger:             logger.CreateLoggerForTest(t).Named("mockProxy"),
		BackendConnManager: NewBackendConnManager(logger.CreateLoggerForTest(t), 0),
	}
	mp.cmdProcessor.capability = cfg.capability
	return mp
}

func (mp *mockProxy) authenticateFirstTime(clientIO, backendIO *pnet.PacketIO) error {
	return mp.authenticator.handshakeFirstTime(mp.logger, clientIO, backendIO, mp.frontendTLSConfig, mp.backendTLSConfig, false)
}

func (mp *mockProxy) authenticateSecondTime(_, backendIO *pnet.PacketIO) error {
	return mp.authenticator.handshakeSecondTime(backendIO, mp.sessionToken)
}

func (mp *mockProxy) processCmd(clientIO, backendIO *pnet.PacketIO) error {
	clientIO.ResetSequence()
	request, err := clientIO.ReadPacket()
	if err != nil {
		return err
	}
	if mp.holdRequest, err = mp.cmdProcessor.executeCmd(request, clientIO, backendIO, mp.waitRedirect); err != nil {
		return err
	}
	// Pretend to redirect the held request to the new backend. The backend must respond for another loop.
	if mp.holdRequest {
		_, err = mp.cmdProcessor.executeCmd(request, clientIO, backendIO, false)
	}
	return err
}

func (mp *mockProxy) directQuery(_, backendIO *pnet.PacketIO) error {
	rs, _, err := mp.cmdProcessor.query(backendIO, mockCmdStr)
	mp.rs = rs
	return err
}
