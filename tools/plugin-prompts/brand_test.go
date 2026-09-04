package main

import "testing"

func TestStoreDefaultsAndBrandNoun(t *testing.T) {
	base := &target{Name: "b", Surface: "memory"}
	applyStoreDefaults(base)
	if base.Store != "soul" || base.Stores != "souls" || base.StoreTitle != "Soul" {
		t.Fatalf("defaults = %q %q %q", base.Store, base.Stores, base.StoreTitle)
	}
	multi := &target{Store: "âme"}
	applyStoreDefaults(multi)
	if multi.StoreTitle != "Âme" {
		t.Fatalf("multi-byte title = %q", multi.StoreTitle)
	}
	bt := brandTarget(&brand{Name: "x", Base: "b", PluginName: "x", Stores: "vaults"}, base)
	if bt.Store != "soul" || bt.Stores != "vaults" {
		t.Fatalf("plural-only override = %q %q", bt.Store, bt.Stores)
	}
	bt = brandTarget(&brand{Name: "y", Base: "b", PluginName: "y", Store: "vault"}, base)
	if bt.Store != "vault" || bt.Stores != "vaults" || bt.StoreTitle != "Vault" {
		t.Fatalf("singular-only override = %q %q %q", bt.Store, bt.Stores, bt.StoreTitle)
	}
}
