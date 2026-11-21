package clienttoken_test

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
)

func nightly() clienttoken.Client {
	return clienttoken.Client{Name: "nightly", Connectors: []string{"github", "linear"}}
}

// clock returns a now func that answers each time in turn, then the last.
func clock(times ...time.Time) func() time.Time {
	var mu sync.Mutex

	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		now := times[0]

		if len(times) > 1 {
			times = times[1:]
		}

		return now
	}
}

func TestRegistryLookup(t *testing.T) {
	d := clienttoken.Digest("wf_s_nightly")
	r := clienttoken.NewRegistry(map[[sha256.Size]byte]clienttoken.Client{d: nightly()}, nil)

	got, ok := r.Lookup(d)

	if !ok {
		t.Fatal("Lookup found no client for a registered digest")
	}

	if diff := cmp.Diff(nightly(), got); diff != "" {
		t.Errorf("Lookup (-want +got):\n%s", diff)
	}

	if _, ok := r.Lookup(clienttoken.Digest("wf_s_other")); ok {
		t.Error("Lookup found a client for an unknown digest")
	}
}

func TestRegistryLookupReturnsCopy(t *testing.T) {
	d := clienttoken.Digest("wf_s_nightly")
	c := nightly()
	r := clienttoken.NewRegistry(map[[sha256.Size]byte]clienttoken.Client{d: c}, nil)
	c.Connectors[0] = "gmail" // the caller's map value must not reach r

	got, _ := r.Lookup(d)
	got.Connectors[1] = "slack"

	again, _ := r.Lookup(d)

	if diff := cmp.Diff(nightly(), again); diff != "" {
		t.Errorf("registry changed through a caller's slice (-want +got):\n%s", diff)
	}
}

func TestRegistryAddRemove(t *testing.T) {
	r := clienttoken.NewRegistry(nil, nil)
	d := clienttoken.Digest("wf_s_nightly")

	if _, ok := r.Lookup(d); ok {
		t.Fatal("empty registry found a client")
	}

	r.Add(d, nightly())

	if got, ok := r.Lookup(d); !ok || got.Name != "nightly" {
		t.Fatalf("after Add: Lookup = %v, %v; want nightly", got, ok)
	}

	if !r.Remove("nightly") {
		t.Error("Remove(nightly) = false, want true")
	}

	if _, ok := r.Lookup(d); ok {
		t.Error("Lookup found a removed token")
	}

	if r.Remove("nightly") {
		t.Error("second Remove(nightly) = true, want false")
	}
}

// TestRegistryAddReplacesName: a token re-created under the same name stops
// the old value from resolving.
func TestRegistryAddReplacesName(t *testing.T) {
	r := clienttoken.NewRegistry(nil, nil)
	oldD, newD := clienttoken.Digest("wf_s_old"), clienttoken.Digest("wf_s_new")
	r.Add(oldD, nightly())
	r.Add(newD, nightly())

	if _, ok := r.Lookup(oldD); ok {
		t.Error("the replaced value still resolves")
	}

	if _, ok := r.Lookup(newD); !ok {
		t.Error("the new value does not resolve")
	}

	if !r.Remove("nightly") {
		t.Fatal("Remove(nightly) = false")
	}

	if _, ok := r.Lookup(newD); ok {
		t.Error("the new value resolves after Remove")
	}
}

func TestRegistryTouches(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 22, 0, 4, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	d := clienttoken.Digest("wf_s_nightly")
	r := clienttoken.NewRegistry(map[[sha256.Size]byte]clienttoken.Client{d: nightly()}, clock(t0, t1))

	if got := r.Touches(); len(got) != 0 {
		t.Errorf("Touches before any Lookup = %v, want none", got)
	}

	r.Lookup(clienttoken.Digest("wf_s_unknown"))
	r.Lookup(d)
	r.Lookup(d)

	if diff := cmp.Diff(map[[sha256.Size]byte]time.Time{d: t1}, r.Touches()); diff != "" {
		t.Errorf("Touches (-want +got):\n%s", diff)
	}

	if got := r.Touches(); len(got) != 0 {
		t.Errorf("Touches after a drain = %v, want none", got)
	}
}

// TestRegistryTouchesKeepTheirToken: a use of a token revoked and created
// again under the same name stays the old token's, keyed by its digest.
func TestRegistryTouchesKeepTheirToken(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 22, 0, 4, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	oldD, newD := clienttoken.Digest("wf_s_old"), clienttoken.Digest("wf_s_new")
	r := clienttoken.NewRegistry(map[[sha256.Size]byte]clienttoken.Client{oldD: nightly()}, clock(t0, t1))

	r.Lookup(oldD)
	r.Remove("nightly")
	r.Add(newD, nightly())
	r.Lookup(newD)

	want := map[[sha256.Size]byte]time.Time{oldD: t0, newD: t1}

	if diff := cmp.Diff(want, r.Touches()); diff != "" {
		t.Errorf("Touches (-want +got):\n%s", diff)
	}
}

// TestRegistryHoldsOnlyScopedTokens: a Client that would pass for the
// default token, by having no connector limit or by its name, never
// resolves, whether it comes through NewRegistry or Add.
func TestRegistryHoldsOnlyScopedTokens(t *testing.T) {
	unlimited := clienttoken.Client{Name: "nightly"}
	named := clienttoken.Client{Name: clienttoken.DefaultName, Connectors: []string{"github"}}
	dUnlimited, dNamed := clienttoken.Digest("wf_s_unlimited"), clienttoken.Digest("wf_s_named")

	fromNew := clienttoken.NewRegistry(map[[sha256.Size]byte]clienttoken.Client{dUnlimited: unlimited, dNamed: named}, nil)
	fromAdd := clienttoken.NewRegistry(nil, nil)
	fromAdd.Add(dUnlimited, unlimited)
	fromAdd.Add(dNamed, named)

	for name, r := range map[string]*clienttoken.Registry{"NewRegistry": fromNew, "Add": fromAdd} {
		for _, d := range [][sha256.Size]byte{dUnlimited, dNamed} {
			if c, ok := r.Lookup(d); ok {
				t.Errorf("%s: Lookup = %+v, want no hit", name, c)
			}
		}

		if r.Remove(clienttoken.DefaultName) {
			t.Errorf("%s: Remove(default) = true, want nothing to remove", name)
		}
	}
}

// TestRegistryConcurrent exercises every method at once; it proves nothing
// without -race (make race).
func TestRegistryConcurrent(t *testing.T) {
	r := clienttoken.NewRegistry(nil, nil)

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Go(func() {
			name := fmt.Sprintf("t%d", i)
			d := clienttoken.Digest("wf_s_" + name)

			for range 100 {
				r.Add(d, clienttoken.Client{Name: name, Connectors: []string{"github"}})

				if c, ok := r.Lookup(d); ok {
					c.Connectors[0] = "changed"
				}

				r.Touches()
				r.Remove(name)
			}
		})
	}

	wg.Wait()

	for i := range 8 {
		if _, ok := r.Lookup(clienttoken.Digest(fmt.Sprintf("wf_s_t%d", i))); ok {
			t.Errorf("t%d still resolves after its last Remove", i)
		}
	}
}
