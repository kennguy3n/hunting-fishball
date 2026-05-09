package policy_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestEffectiveMode_PicksStricter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tenant, channel, want policy.PrivacyMode
	}{
		{policy.PrivacyModeNoAI, policy.PrivacyModeRemote, policy.PrivacyModeNoAI},
		{policy.PrivacyModeRemote, policy.PrivacyModeLocalOnly, policy.PrivacyModeLocalOnly},
		{policy.PrivacyModeHybrid, policy.PrivacyModeHybrid, policy.PrivacyModeHybrid},
		{policy.PrivacyModeLocalAPI, policy.PrivacyModeHybrid, policy.PrivacyModeLocalAPI},
		{policy.PrivacyModeLocalOnly, policy.PrivacyModeNoAI, policy.PrivacyModeNoAI},
	}
	for _, c := range cases {
		got := policy.EffectiveMode(c.tenant, c.channel)
		if got != c.want {
			t.Fatalf("EffectiveMode(%q,%q)=%q want %q", c.tenant, c.channel, got, c.want)
		}
	}
}

func TestEffectiveMode_UnknownFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		tenant, channel policy.PrivacyMode
	}{
		{"both empty", "", ""},
		{"unknown tenant + Remote channel", policy.PrivacyMode("rogue"), policy.PrivacyModeRemote},
		{"unknown tenant + LocalOnly channel", policy.PrivacyMode("rogue"), policy.PrivacyModeLocalOnly},
		{"NoAI tenant + unknown channel", policy.PrivacyModeNoAI, policy.PrivacyMode("rogue")},
		{"Remote tenant + unknown channel", policy.PrivacyModeRemote, policy.PrivacyMode("rogue")},
	}
	for _, c := range cases {
		if got := policy.EffectiveMode(c.tenant, c.channel); got != policy.PrivacyModeNoAI {
			t.Errorf("%s: EffectiveMode(%q,%q)=%q want %q", c.name, c.tenant, c.channel, got, policy.PrivacyModeNoAI)
		}
	}
}

func TestParsePrivacyMode(t *testing.T) {
	t.Parallel()
	cases := map[string]policy.PrivacyMode{
		"no-ai":      policy.PrivacyModeNoAI,
		"NO_AI":      policy.PrivacyModeNoAI,
		" hybrid ":   policy.PrivacyModeHybrid,
		"local_only": policy.PrivacyModeLocalOnly,
	}
	for in, want := range cases {
		got, ok := policy.ParsePrivacyMode(in)
		if !ok || got != want {
			t.Fatalf("ParsePrivacyMode(%q)=%q ok=%v want %q ok=true", in, got, ok, want)
		}
	}
	if _, ok := policy.ParsePrivacyMode("nope"); ok {
		t.Fatal("unknown mode must not parse")
	}
}

func TestAllowsAtLeast(t *testing.T) {
	t.Parallel()
	if !policy.AllowsAtLeast(policy.PrivacyModeRemote, policy.PrivacyModeHybrid) {
		t.Fatal("Remote must satisfy Hybrid")
	}
	if policy.AllowsAtLeast(policy.PrivacyModeLocalOnly, policy.PrivacyModeRemote) {
		t.Fatal("LocalOnly must not satisfy Remote")
	}
	if policy.AllowsAtLeast(policy.PrivacyMode("nope"), policy.PrivacyModeRemote) {
		t.Fatal("unknown actual must fail closed")
	}
}

func TestAllPrivacyModes_Order(t *testing.T) {
	t.Parallel()
	got := policy.AllPrivacyModes()
	want := []policy.PrivacyMode{
		policy.PrivacyModeNoAI,
		policy.PrivacyModeLocalOnly,
		policy.PrivacyModeLocalAPI,
		policy.PrivacyModeHybrid,
		policy.PrivacyModeRemote,
	}
	if len(got) != len(want) {
		t.Fatalf("len: got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("position %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}
