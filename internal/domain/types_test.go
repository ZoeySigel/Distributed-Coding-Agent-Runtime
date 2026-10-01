package domain

import "testing"

func TestNormalizeRejectsCredentialAndSSRFURLs(t *testing.T) {
	for _, u := range []string{"http://github.com/a/b", "https://github.com.evil/a/b", "https://token@github.com/a/b", "https://github.com:443/a/b", "https://127.0.0.1/a/b", "https://github.com/a/../b", "https://github.com/a/b?x=1", "https://github.com/a/%62", "file:///repo"} {
		s := Spec{Repository: u, Prompt: "fix"}
		if s.Normalize() == nil {
			t.Errorf("accepted %s", u)
		}
	}
}
func TestNormalizeDefaultsAndRefs(t *testing.T) {
	s := Spec{Repository: "https://github.com/org/repo/", Prompt: "fix"}
	if e := s.Normalize(); e != nil {
		t.Fatal(e)
	}
	if s.Repository != "https://github.com/org/repo.git" || s.TimeoutSeconds != 3600 || s.TestTimeoutSeconds != 600 || s.Profile != "default" {
		t.Fatalf("bad defaults %+v", s)
	}
	for _, ref := range []string{"--upload-pack=evil", "main\nother", "main~1", "a:b"} {
		s.Ref = ref
		if s.Normalize() == nil {
			t.Fatalf("accepted ref %q", ref)
		}
	}
}
