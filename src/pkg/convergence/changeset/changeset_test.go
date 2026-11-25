package changeset

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/convergence/proof"
)

func sha(c string) string { return strings.Repeat(c, 40) }

func member(repo, c string) Member { return Member{Repo: repo, PR: 1, HeadSHA: sha(c)} }

func edge(p, pc, c, cc string) Edge {
	return Edge{Producer: p, Consumer: c, Compatibility: Compatibility{
		Scenario: "just test-compat", ProducerHeadSHA: sha(pc), ConsumerHeadSHA: sha(cc)}}
}

func TestRejections(t *testing.T) {
	a, b := member("o/a", "a"), member("o/b", "b")
	cases := map[string]struct {
		spec Spec
		want string
	}{
		"self":       {Spec{"x", []Member{a}, []Edge{edge("o/a", "a", "o/a", "a")}}, "depend on itself"},
		"missing":    {Spec{"x", []Member{a, b}, []Edge{edge("o/a", "a", "o/zzz", "b")}}, "undeclared participant"},
		"duplicate":  {Spec{"x", []Member{a, b}, []Edge{edge("o/a", "a", "o/b", "b"), edge("o/a", "a", "o/b", "b")}}, "duplicate dependency"},
		"dup member": {Spec{"x", []Member{a, a}, nil}, "duplicate member"},
		"cycle":      {Spec{"x", []Member{a, b}, []Edge{edge("o/a", "a", "o/b", "b"), edge("o/b", "b", "o/a", "a")}}, "dependency cycle"},
		"no scenario": {Spec{"x", []Member{a, b}, []Edge{{Producer: "o/a", Consumer: "o/b",
			Compatibility: Compatibility{ProducerHeadSHA: sha("a"), ConsumerHeadSHA: sha("b")}}}}, "no compatibility scenario"},
		"stale producer sha": {Spec{"x", []Member{a, b}, []Edge{edge("o/a", "c", "o/b", "b")}}, "producer head"},
		"stale consumer sha": {Spec{"x", []Member{a, b}, []Edge{edge("o/a", "a", "o/b", "c")}}, "consumer head"},
		"short sha":          {Spec{"x", []Member{{Repo: "o/a", PR: 1, HeadSHA: "abc"}}, nil}, "40-hex"},
		"no name":            {Spec{"", []Member{a}, nil}, "requires a name"},
	}
	for name, c := range cases {
		_, err := Build(c.spec)
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Error(), c.want) {
			t.Errorf("%s: got %v, want reason %q", name, err, c.want)
		}
	}
}

func TestPlanLayersConcurrencyAndBlocking(t *testing.T) {
	fish := member("tuna-os/fisherman", "1")
	fish.Published, fish.Proof = true, proof.ResultSuccess
	k, g := member("tuna-os/bootc-installer-kde", "2"), member("tuna-os/bootc-installer-gnome", "3")
	pkg, img := member("tuna-os/tunaos-packages", "4"), member("tuna-os/tunaOS", "5")
	pkg.Proof = proof.ResultSuccess // verified but unpublished
	plan, err := Build(Spec{Name: "installer", Members: []Member{img, k, fish, g, pkg}, Edges: []Edge{
		edge("tuna-os/fisherman", "1", "tuna-os/bootc-installer-kde", "2"),
		edge("tuna-os/fisherman", "1", "tuna-os/bootc-installer-gnome", "3"),
		edge("tuna-os/tunaos-packages", "4", "tuna-os/tunaOS", "5"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantLayers := [][]string{
		{"tuna-os/fisherman", "tuna-os/tunaos-packages"},
		{"tuna-os/bootc-installer-gnome", "tuna-os/bootc-installer-kde", "tuna-os/tunaOS"},
	}
	if !reflect.DeepEqual(plan.Layers, wantLayers) {
		t.Fatalf("layers %v", plan.Layers)
	}
	wantReady := []string{"tuna-os/bootc-installer-gnome", "tuna-os/bootc-installer-kde", "tuna-os/fisherman", "tuna-os/tunaos-packages"}
	if !reflect.DeepEqual(plan.Ready, wantReady) {
		t.Fatalf("ready %v", plan.Ready)
	}
	if len(plan.Blocked) != 1 || plan.Blocked[0].Repo != "tuna-os/tunaOS" ||
		len(plan.Blocked[0].Reasons) != 1 || !strings.Contains(plan.Blocked[0].Reasons[0], "not published") {
		t.Fatalf("blocked %+v", plan.Blocked)
	}
}

func TestUnverifiedProducerBlocks(t *testing.T) {
	p, c := member("o/p", "a"), member("o/c", "b")
	p.Published = true // proof missing
	plan, err := Build(Spec{Name: "x", Members: []Member{p, c}, Edges: []Edge{edge("o/p", "a", "o/c", "b")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blocked) != 1 || !strings.Contains(plan.Blocked[0].Reasons[0], "not verified") {
		t.Fatalf("blocked %+v", plan.Blocked)
	}
}
