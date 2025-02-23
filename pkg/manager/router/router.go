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

package router

import (
	"container/list"
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/pingcap/TiProxy/lib/config"
	"github.com/pingcap/TiProxy/lib/util/errors"
	"github.com/pingcap/TiProxy/lib/util/waitgroup"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// Router routes client connections to backends.
type Router interface {
	Route(RedirectableConn) (string, error)
	RedirectConnections() error
	Close()
}

var (
	ErrNoInstanceToSelect = errors.New("no instances to route")
)

type connPhase int

const (
	// The session is never redirected.
	phaseNotRedirected connPhase = iota
	// The session is redirecting.
	phaseRedirectNotify
	// The session redirected successfully last time.
	phaseRedirectEnd
	// The session failed to redirect last time.
	phaseRedirectFail
)

const (
	// The interval to rebalance connections.
	rebalanceInterval = 10 * time.Millisecond
	// The number of connections to rebalance during each interval.
	// Limit the number to avoid creating too many connections suddenly on a backend.
	rebalanceConnsPerLoop = 10
	// The threshold of ratio of the highest score and lowest score.
	// If the ratio exceeds the threshold, the proxy will rebalance connections.
	rebalanceMaxScoreRatio = 1.2
	// After a connection fails to redirect, it may contain some unmigratable status.
	// Limit its redirection interval to avoid unnecessary retrial to reduce latency jitter.
	redirectFailMinInterval = 3 * time.Second
)

// ConnEventReceiver receives connection events.
type ConnEventReceiver interface {
	OnRedirectSucceed(from, to string, conn RedirectableConn) error
	OnRedirectFail(from, to string, conn RedirectableConn) error
	OnConnClosed(addr string, conn RedirectableConn) error
}

// RedirectableConn indicates a redirect-able connection.
type RedirectableConn interface {
	SetEventReceiver(receiver ConnEventReceiver)
	Redirect(addr string)
	GetRedirectingAddr() string
	ConnectionID() uint64
}

// backendWrapper contains the connections on the backend.
type backendWrapper struct {
	status BackendStatus
	addr   string
	// A list of *connWrapper and is ordered by the connecting or redirecting time.
	// connList and connMap include moving out connections but not moving in connections.
	connList *list.List
	connMap  map[uint64]*list.Element
}

// score calculates the score of the backend. Larger score indicates higher load.
func (b *backendWrapper) score() int {
	return b.status.ToScore() + b.connList.Len()
}

// connWrapper wraps RedirectableConn.
type connWrapper struct {
	RedirectableConn
	phase connPhase
	// Last redirect start time of this connection.
	lastRedirect time.Time
}

// ScoreBasedRouter is an implementation of Router interface.
// It routes a connection based on score.
type ScoreBasedRouter struct {
	sync.Mutex
	logger     *zap.Logger
	observer   *BackendObserver
	cancelFunc context.CancelFunc
	wg         waitgroup.WaitGroup
	// A list of *backendWrapper. The backends are in descending order of scores.
	backends *list.List
}

// NewScoreBasedRouter creates a ScoreBasedRouter.
func NewScoreBasedRouter(logger *zap.Logger, cfg *config.BackendNamespace, client *clientv3.Client, httpCli *http.Client) (*ScoreBasedRouter, error) {
	router := &ScoreBasedRouter{
		logger:   logger,
		backends: list.New(),
	}
	router.Lock()
	defer router.Unlock()
	observer, err := StartBackendObserver(logger.Named("observer"), router, client, httpCli, newDefaultHealthCheckConfig(), cfg.Instances)
	if err != nil {
		return nil, err
	}
	router.observer = observer
	childCtx, cancelFunc := context.WithCancel(context.Background())
	router.cancelFunc = cancelFunc
	router.wg.Run(func() {
		router.rebalanceLoop(childCtx)
	})
	return router, nil
}

// Route implements Router.Route interface.
func (router *ScoreBasedRouter) Route(conn RedirectableConn) (string, error) {
	router.Lock()
	defer router.Unlock()
	be := router.backends.Back()
	if be == nil {
		return "", ErrNoInstanceToSelect
	}
	backend := be.Value.(*backendWrapper)
	switch backend.status {
	case StatusCannotConnect, StatusSchemaOutdated:
		return "", ErrNoInstanceToSelect
	}
	connWrapper := &connWrapper{
		RedirectableConn: conn,
		phase:            phaseNotRedirected,
	}
	router.addConn(be, connWrapper)
	addBackendConnMetrics(backend.addr)
	conn.SetEventReceiver(router)
	return backend.addr, nil
}

func (router *ScoreBasedRouter) removeConn(be *list.Element, ce *list.Element) {
	backend := be.Value.(*backendWrapper)
	conn := ce.Value.(*connWrapper)
	backend.connList.Remove(ce)
	delete(backend.connMap, conn.ConnectionID())
	if !router.removeBackendIfEmpty(be) {
		router.adjustBackendList(be)
	}
}

func (router *ScoreBasedRouter) addConn(be *list.Element, conn *connWrapper) {
	backend := be.Value.(*backendWrapper)
	ce := backend.connList.PushBack(conn)
	backend.connMap[conn.ConnectionID()] = ce
	router.adjustBackendList(be)
}

// adjustBackendList moves `be` after the score of `be` changes to keep the list ordered.
func (router *ScoreBasedRouter) adjustBackendList(be *list.Element) {
	backend := be.Value.(*backendWrapper)
	curScore := backend.score()
	var mark *list.Element
	for ele := be.Prev(); ele != nil; ele = ele.Prev() {
		b := ele.Value.(*backendWrapper)
		if b.score() >= curScore {
			break
		}
		mark = ele
	}
	if mark != nil {
		router.backends.MoveBefore(be, mark)
		return
	}
	for ele := be.Next(); ele != nil; ele = ele.Next() {
		b := ele.Value.(*backendWrapper)
		if b.score() <= curScore {
			break
		}
		mark = ele
	}
	if mark != nil {
		router.backends.MoveAfter(be, mark)
	}
}

// RedirectConnections implements Router.RedirectConnections interface.
// It redirects all connections compulsively. It's only used for testing.
func (router *ScoreBasedRouter) RedirectConnections() error {
	router.Lock()
	defer router.Unlock()
	for be := router.backends.Front(); be != nil; be = be.Next() {
		backend := be.Value.(*backendWrapper)
		for ce := backend.connList.Front(); ce != nil; ce = ce.Next() {
			// This is only for test, so we allow it to reconnect to the same backend.
			connWrapper := ce.Value.(*connWrapper)
			if connWrapper.phase != phaseRedirectNotify {
				connWrapper.phase = phaseRedirectNotify
				connWrapper.Redirect(backend.addr)
			}
		}
	}
	return nil
}

// forward is a hint to speed up searching.
func (router *ScoreBasedRouter) lookupBackend(addr string, forward bool) *list.Element {
	if forward {
		for be := router.backends.Front(); be != nil; be = be.Next() {
			backend := be.Value.(*backendWrapper)
			if backend.addr == addr {
				return be
			}
		}
	} else {
		for be := router.backends.Back(); be != nil; be = be.Prev() {
			backend := be.Value.(*backendWrapper)
			if backend.addr == addr {
				return be
			}
		}
	}
	return nil
}

// OnRedirectSucceed implements ConnEventReceiver.OnRedirectSucceed interface.
func (router *ScoreBasedRouter) OnRedirectSucceed(from, to string, conn RedirectableConn) error {
	router.Lock()
	defer router.Unlock()
	be := router.lookupBackend(to, false)
	if be == nil {
		return errors.WithStack(errors.Errorf("backend %s is not found in the router", to))
	}
	toBackend := be.Value.(*backendWrapper)
	e, ok := toBackend.connMap[conn.ConnectionID()]
	if !ok {
		return errors.WithStack(errors.Errorf("connection %d is not found on the backend %s", conn.ConnectionID(), to))
	}
	connWrapper := e.Value.(*connWrapper)
	connWrapper.phase = phaseRedirectEnd
	addMigrateMetrics(from, to, true, connWrapper.lastRedirect)
	subBackendConnMetrics(from)
	addBackendConnMetrics(to)
	return nil
}

// OnRedirectFail implements ConnEventReceiver.OnRedirectFail interface.
func (router *ScoreBasedRouter) OnRedirectFail(from, to string, conn RedirectableConn) error {
	router.Lock()
	defer router.Unlock()
	be := router.lookupBackend(to, false)
	if be == nil {
		return errors.WithStack(errors.Errorf("backend %s is not found in the router", to))
	}
	toBackend := be.Value.(*backendWrapper)
	ce, ok := toBackend.connMap[conn.ConnectionID()]
	if !ok {
		return errors.WithStack(errors.Errorf("connection %d is not found on the backend %s", conn.ConnectionID(), to))
	}
	router.removeConn(be, ce)

	be = router.lookupBackend(from, true)
	// The backend may have been removed because it's empty. Add it back.
	if be == nil {
		be = router.backends.PushBack(&backendWrapper{
			status:   StatusCannotConnect,
			addr:     from,
			connList: list.New(),
			connMap:  make(map[uint64]*list.Element),
		})
	}
	connWrapper := ce.Value.(*connWrapper)
	connWrapper.phase = phaseRedirectFail
	addMigrateMetrics(from, to, false, connWrapper.lastRedirect)
	router.addConn(be, connWrapper)
	return nil
}

// OnConnClosed implements ConnEventReceiver.OnConnClosed interface.
func (router *ScoreBasedRouter) OnConnClosed(addr string, conn RedirectableConn) error {
	router.Lock()
	defer router.Unlock()
	// Get the redirecting address in the lock, rather than letting the connection pass it in.
	// While the connection closes, the router may also send a new redirection signal concurrently
	// and move it to another backendWrapper.
	if toAddr := conn.GetRedirectingAddr(); len(toAddr) > 0 {
		addr = toAddr
	}
	be := router.lookupBackend(addr, true)
	if be == nil {
		return errors.WithStack(errors.Errorf("backend %s is not found in the router", addr))
	}
	backend := be.Value.(*backendWrapper)
	ce, ok := backend.connMap[conn.ConnectionID()]
	if !ok {
		return errors.WithStack(errors.Errorf("connection %d is not found on the backend %s", conn.ConnectionID(), addr))
	}
	router.removeConn(be, ce)
	subBackendConnMetrics(addr)
	return nil
}

// OnBackendChanged implements BackendEventReceiver.OnBackendChanged interface.
func (router *ScoreBasedRouter) OnBackendChanged(backends map[string]BackendStatus) {
	router.Lock()
	defer router.Unlock()
	for addr, status := range backends {
		be := router.lookupBackend(addr, true)
		if be == nil && status != StatusCannotConnect {
			router.logger.Info("find new backend", zap.String("addr", addr),
				zap.String("status", status.String()))
			be = router.backends.PushBack(&backendWrapper{
				status:   status,
				addr:     addr,
				connList: list.New(),
				connMap:  make(map[uint64]*list.Element),
			})
		} else {
			backend := be.Value.(*backendWrapper)
			router.logger.Info("update backend", zap.String("addr", addr),
				zap.String("prev_status", backend.status.String()), zap.String("cur_status", status.String()))
			backend.status = status
		}
		if !router.removeBackendIfEmpty(be) {
			router.adjustBackendList(be)
		}
	}
}

func (router *ScoreBasedRouter) rebalanceLoop(ctx context.Context) {
	for {
		router.rebalance(rebalanceConnsPerLoop)
		select {
		case <-ctx.Done():
			return
		case <-time.After(rebalanceInterval):
		}
	}
}

func (router *ScoreBasedRouter) rebalance(maxNum int) {
	curTime := time.Now()
	router.Lock()
	defer router.Unlock()
	for i := 0; i < maxNum; i++ {
		var busiestEle *list.Element
		for be := router.backends.Front(); be != nil; be = be.Next() {
			backend := be.Value.(*backendWrapper)
			if backend.connList.Len() > 0 {
				busiestEle = be
				break
			}
		}
		if busiestEle == nil {
			break
		}
		busiestBackend := busiestEle.Value.(*backendWrapper)
		idlestEle := router.backends.Back()
		idlestBackend := idlestEle.Value.(*backendWrapper)
		if float64(busiestBackend.score())/float64(idlestBackend.score()+1) < rebalanceMaxScoreRatio {
			break
		}
		var ce *list.Element
		for ele := busiestBackend.connList.Front(); ele != nil; ele = ele.Next() {
			conn := ele.Value.(*connWrapper)
			switch conn.phase {
			case phaseRedirectNotify:
				// A connection cannot be redirected again when it has not finished redirecting.
				continue
			case phaseRedirectFail:
				// If it failed recently, it will probably fail this time.
				if conn.lastRedirect.Add(redirectFailMinInterval).After(curTime) {
					continue
				}
			}
			ce = ele
			break
		}
		if ce == nil {
			break
		}
		router.removeConn(busiestEle, ce)
		conn := ce.Value.(*connWrapper)
		conn.phase = phaseRedirectNotify
		conn.lastRedirect = curTime
		router.addConn(idlestEle, conn)
		conn.Redirect(idlestBackend.addr)
	}
}

func (router *ScoreBasedRouter) removeBackendIfEmpty(be *list.Element) bool {
	backend := be.Value.(*backendWrapper)
	if backend.status == StatusCannotConnect && backend.connList.Len() == 0 {
		router.backends.Remove(be)
		return true
	}
	return false
}

// Close implements Router.Close interface.
func (router *ScoreBasedRouter) Close() {
	router.Lock()
	defer router.Unlock()
	if router.cancelFunc != nil {
		router.cancelFunc()
		router.cancelFunc = nil
	}
	if router.observer != nil {
		router.observer.Close()
		router.observer = nil
	}
	router.wg.Wait()
	// Router only refers to RedirectableConn, it doesn't manage RedirectableConn.
}
