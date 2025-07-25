// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kvstore

import (
	"context"
	"maps"
	"testing"

	"github.com/cilium/hive/hivetest"

	"github.com/cilium/cilium/pkg/time"
)

var (
	// etcdDummyAddress can be overwritten from test invokers using ldflags
	etcdDummyAddress = "http://127.0.0.1:4002"
)

// SetupDummy sets up kvstore for tests. A lock mechanism it used to prevent
// the creation of two clients at the same time, to avoid interferences in case
// different tests are run in parallel. A cleanup function is automatically
// registered to delete all keys and close the client when the test terminates.
func SetupDummy(tb testing.TB, dummyBackend string) Client {
	return SetupDummyWithConfigOpts(tb, dummyBackend, nil)
}

// SetupDummyWithConfigOpts sets up the dummy kvstore for tests but also
// configures the module with the provided opts. A lock mechanism it used to
// prevent the creation of two clients at the same time, to avoid interferences
// in case different tests are run in parallel. A cleanup function is
// automatically registered to delete all keys and close the client when the
// test terminates.
func SetupDummyWithConfigOpts(tb testing.TB, dummyBackend string, opts map[string]string) Client {
	if dummyBackend == DisabledBackendName {
		return &clientImpl{enabled: false}
	}

	module := getBackend(dummyBackend)
	if module == nil {
		tb.Fatalf("Unknown dummy kvstore backend %s", dummyBackend)
	}

	switch dummyBackend {
	case EtcdBackendName:
		if opts == nil {
			opts = make(map[string]string)
		} else {
			opts = maps.Clone(opts)
		}
		opts[EtcdAddrOption] = EtcdDummyAddress()
	}

	err := module.setConfig(hivetest.Logger(tb), opts)
	if err != nil {
		tb.Fatalf("Unable to set config options for kvstore backend module: %v", err)
	}

	client, errCh := module.newClient(context.Background(), hivetest.Logger(tb), ExtraOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	select {
	case err = <-errCh:
	case <-ctx.Done():
		err = ctx.Err()
	}

	if err != nil {
		if client != nil {
			client.Close()
		}

		tb.Fatalf("Failed waiting for kvstore connection to be established: %v", err)
	}

	tb.Cleanup(func() {
		if err := client.DeletePrefix(context.Background(), ""); err != nil {
			tb.Fatalf("Unable to delete all kvstore keys: %v", err)
		}

		client.Close()
	})

	// Multiple tests might be running in parallel by go test if they are part of
	// different packages. Let's implement a locking mechanism to ensure that only
	// one at a time can access the kvstore, to prevent that they interact with
	// each other. Locking is implemented through CreateOnly (rather than using
	// the locking abstraction), so that we can release it in the same atomic
	// transaction that also removes all the other keys.
	for {
		succeeded, err := client.CreateOnly(ctx, ".lock", []byte(""), true)
		if err != nil {
			tb.Fatalf("Unable to acquire the kvstore lock: %v", err)
		}

		if succeeded {
			return &clientImpl{enabled: true, BackendOperations: client}
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			tb.Fatal("Timed out waiting to acquire the kvstore lock")
		}
	}
}

func EtcdDummyAddress() string {
	return etcdDummyAddress
}
