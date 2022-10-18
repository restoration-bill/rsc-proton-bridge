// Copyright (c) 2022 Proton AG
//
// This file is part of Proton Mail Bridge.
//
// Proton Mail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Proton Mail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with Proton Mail Bridge.  If not, see <https://www.gnu.org/licenses/>.

package tests

import (
	"context"
	"fmt"
	"net/smtp"
	"regexp"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/ProtonMail/gluon/queue"
	"github.com/ProtonMail/proton-bridge/v2/internal/bridge"
	"github.com/ProtonMail/proton-bridge/v2/internal/events"
	"github.com/ProtonMail/proton-bridge/v2/internal/locations"
	"github.com/bradenaw/juniper/xslices"
	"github.com/emersion/go-imap/client"
	"gitlab.protontech.ch/go/liteapi"
	"gitlab.protontech.ch/go/liteapi/server"
	"golang.org/x/exp/maps"
)

var defaultVersion = semver.MustParse("1.0.0")

type testCtx struct {
	// These are the objects supporting the test.
	dir      string
	api      API
	netCtl   *liteapi.NetCtl
	locator  *locations.Locations
	storeKey []byte
	version  *semver.Version
	mocks    *bridge.Mocks

	// bridge holds the bridge app under test.
	bridge *bridge.Bridge

	// These channels hold events of various types coming from bridge.
	loginCh        *queue.QueuedChannel[events.UserLoggedIn]
	logoutCh       *queue.QueuedChannel[events.UserLoggedOut]
	loadedCh       *queue.QueuedChannel[events.AllUsersLoaded]
	deletedCh      *queue.QueuedChannel[events.UserDeleted]
	deauthCh       *queue.QueuedChannel[events.UserDeauth]
	addrCreatedCh  *queue.QueuedChannel[events.UserAddressCreated]
	addrDeletedCh  *queue.QueuedChannel[events.UserAddressDeleted]
	syncStartedCh  *queue.QueuedChannel[events.SyncStarted]
	syncFinishedCh *queue.QueuedChannel[events.SyncFinished]
	forcedUpdateCh *queue.QueuedChannel[events.UpdateForced]
	connStatusCh   *queue.QueuedChannel[events.Event]
	updateCh       *queue.QueuedChannel[events.Event]

	// These maps hold expected userIDByName, their primary addresses and bridge passwords.
	userIDByName       map[string]string
	userAddrByEmail    map[string]map[string]string
	userPassByID       map[string]string
	userBridgePassByID map[string][]byte

	// These are the IMAP and SMTP clients used to connect to bridge.
	imapClients map[string]*imapClient
	smtpClients map[string]*smtpClient

	// calls holds calls made to the API during each step of the test.
	calls [][]server.Call

	// errors holds test-related errors encountered while running test steps.
	errors [][]error
}

type imapClient struct {
	userID string
	client *client.Client
}

type smtpClient struct {
	userID string
	client *smtp.Client
}

func newTestCtx(tb testing.TB) *testCtx {
	dir := tb.TempDir()

	ctx := &testCtx{
		dir:      dir,
		api:      newFakeAPI(),
		netCtl:   liteapi.NewNetCtl(),
		locator:  locations.New(bridge.NewTestLocationsProvider(dir), "config-name"),
		storeKey: []byte("super-secret-store-key"),
		mocks:    bridge.NewMocks(tb, defaultVersion, defaultVersion),
		version:  defaultVersion,

		userIDByName:       make(map[string]string),
		userAddrByEmail:    make(map[string]map[string]string),
		userPassByID:       make(map[string]string),
		userBridgePassByID: make(map[string][]byte),

		imapClients: make(map[string]*imapClient),
		smtpClients: make(map[string]*smtpClient),
	}

	ctx.api.AddCallWatcher(func(call server.Call) {
		ctx.calls[len(ctx.calls)-1] = append(ctx.calls[len(ctx.calls)-1], call)
	})

	return ctx
}

func (t *testCtx) beforeStep() {
	t.calls = append(t.calls, nil)
	t.errors = append(t.errors, nil)
}

func (t *testCtx) getUserID(username string) string {
	return t.userIDByName[username]
}

func (t *testCtx) setUserID(username, userID string) {
	t.userIDByName[username] = userID
}

func (t *testCtx) getUserAddrID(userID, email string) string {
	return t.userAddrByEmail[userID][email]
}

func (t *testCtx) getUserAddrs(userID string) []string {
	return maps.Keys(t.userAddrByEmail[userID])
}

func (t *testCtx) setUserAddr(userID, addrID, email string) {
	if _, ok := t.userAddrByEmail[userID]; !ok {
		t.userAddrByEmail[userID] = make(map[string]string)
	}

	t.userAddrByEmail[userID][email] = addrID
}

func (t *testCtx) unsetUserAddr(userID, wantAddrID string) {
	for email, addrID := range t.userAddrByEmail[userID] {
		if addrID == wantAddrID {
			delete(t.userAddrByEmail[userID], email)
		}
	}
}

func (t *testCtx) getUserPass(userID string) string {
	return t.userPassByID[userID]
}

func (t *testCtx) setUserPass(userID, pass string) {
	t.userPassByID[userID] = pass
}

func (t *testCtx) getUserBridgePass(userID string) string {
	return string(t.userBridgePassByID[userID])
}

func (t *testCtx) setUserBridgePass(userID string, pass []byte) {
	t.userBridgePassByID[userID] = pass
}

func (t *testCtx) getMBoxID(userID string, name string) string {
	labels, err := t.api.GetLabels(userID)
	if err != nil {
		panic(err)
	}

	idx := xslices.IndexFunc(labels, func(label liteapi.Label) bool {
		return label.Name == name
	})

	if idx < 0 {
		panic(fmt.Errorf("label %q not found", name))
	}

	return labels[idx].ID
}

func (t *testCtx) getLastCall(method, path string) (server.Call, error) {
	var allCalls []server.Call

	for _, calls := range t.calls {
		allCalls = append(allCalls, calls...)
	}

	if len(allCalls) == 0 {
		return server.Call{}, fmt.Errorf("no calls made")
	}

	for idx := len(allCalls) - 1; idx >= 0; idx-- {
		if call := allCalls[idx]; call.Method == method && regexp.MustCompile("^"+path+"$").MatchString(call.URL.Path) {
			return call, nil
		}
	}

	return server.Call{}, fmt.Errorf("no call with method %q and path %q was made", method, path)
}

func (t *testCtx) pushError(err error) {
	t.errors[len(t.errors)-1] = append(t.errors[len(t.errors)-1], err)
}

func (t *testCtx) getLastError() error {
	errors := t.errors[len(t.errors)-2]

	if len(errors) == 0 {
		return nil
	}

	return errors[len(errors)-1]
}

func (t *testCtx) close(ctx context.Context) error {
	for _, client := range t.imapClients {
		if err := client.client.Logout(); err != nil {
			return err
		}
	}

	if t.bridge != nil {
		if err := t.bridge.Close(ctx); err != nil {
			return err
		}
	}

	t.api.Close()

	return nil
}
