package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"

	"sing-box-plus/internal/minreg"
)

// Reload and check inherit process services but must not leave a Box's mutable
// router/network services in globalCtx for the next instance to reuse.
func TestBoxContextIsolation(t *testing.T) {
	oldCtx, oldPaths, oldDirectories := globalCtx, configPaths, configDirectories
	oldRegistry, oldConfig := statsRegistry, statsConfig
	t.Cleanup(func() {
		globalCtx, configPaths, configDirectories = oldCtx, oldPaths, oldDirectories
		statsRegistry, statsConfig = oldRegistry, oldConfig
	})
	statsRegistry, statsConfig = nil, nil
	globalCtx = box.Context(context.Background(), minreg.InboundRegistry(), minreg.OutboundRegistry(),
		minreg.EndpointRegistry(), minreg.DNSTransportRegistry(), newServiceRegistry(), minreg.CertificateProviderRegistry())
	assertUnchanged := func() {
		t.Helper()
		if service.FromContext[adapter.Router](globalCtx) != nil || service.FromContext[adapter.NetworkManager](globalCtx) != nil {
			t.Fatal("Box polluted the process service context")
		}
	}
	for range 2 {
		instance, cancel, err := create(option.Options{})
		if err != nil {
			t.Fatal(err)
		}
		assertUnchanged()
		if err = instance.Close(); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	configFile := filepath.Join(t.TempDir(), "check.json")
	if err := os.WriteFile(configFile, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPaths, configDirectories = []string{configFile}, nil
	if err := check(); err != nil {
		t.Fatal(err)
	}
	assertUnchanged()
}
