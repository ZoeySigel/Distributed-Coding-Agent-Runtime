package domain

import (
	"strings"
	"testing"
)

func TestPublicationPathAndSizeBoundaries(t *testing.T) {
	base := ChangeSet{SHA: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)}
	for _, path := range []string{"../outside", "/absolute", "a/.git/config", "a/../b", "a\\b", "a:b", "a//b", "a\x00b"} {
		c := base
		c.Changes = []Change{{Path: path, Mode: "100644"}}
		if c.Validate() == nil {
			t.Fatal("unsafe path accepted", path)
		}
	}
	c := base
	c.Changes = []Change{{Path: "a", Mode: "100755", Content: []byte{0, 1, 2}}, {Path: "old", Mode: "100644", Delete: true}}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.Changes = append(c.Changes, c.Changes[0])
	if c.Validate() == nil {
		t.Fatal("duplicate path accepted")
	}
	c.Changes = []Change{{Path: "a", Mode: "160000"}}
	if c.Validate() == nil {
		t.Fatal("submodule accepted")
	}
	c.Changes = []Change{{Path: "a", Mode: "100644", Content: make([]byte, (16<<20)+1)}}
	if c.Validate() == nil {
		t.Fatal("oversized contents accepted")
	}
}
func TestAutoPRDefaultIsCanonicalAndOptOutSurvives(t *testing.T) {
	yes, no := true, false
	a := Spec{Repository: "https://github.com/team/repo", Prompt: "fix"}
	b := a
	b.AutoPR = &yes
	if e := a.Normalize(); e != nil {
		t.Fatal(e)
	}
	if e := b.Normalize(); e != nil {
		t.Fatal(e)
	}
	if a != b {
		t.Fatal("default idempotency hash changed")
	}
	b.AutoPR = &no
	if e := b.Normalize(); e != nil || b.AutoPR == nil || *b.AutoPR {
		t.Fatal("opt out lost", e)
	}
}
