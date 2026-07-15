package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetPrefersProcessEnv(t *testing.T) {
	t.Setenv("DOTENV_TEST_KEY", "from-env")
	f := writeTemp(t, "DOTENV_TEST_KEY=from-file\n")
	if got := Get("DOTENV_TEST_KEY", f); got != "from-env" {
		t.Errorf("Get = %q, want process env to win", got)
	}
}

func TestGetFallsBackToFile(t *testing.T) {
	f := writeTemp(t, "# comment\n\nFOO=bar\nQUOTED=\"q v\"\nSPACED =  s  \n")
	for key, want := range map[string]string{"FOO": "bar", "QUOTED": "q v", "SPACED": "s"} {
		if got := Get(key, f); got != want {
			t.Errorf("Get(%s) = %q, want %q", key, got, want)
		}
	}
}

func TestGetFirstFileWins(t *testing.T) {
	f1 := writeTemp(t, "K=first\n")
	f2 := writeTemp(t, "K=second\n")
	if got := Get("K", f1, f2); got != "first" {
		t.Errorf("Get = %q, want %q", got, "first")
	}
}

func TestGetMissing(t *testing.T) {
	f := writeTemp(t, "A=1\n")
	if got := Get("NOPE", f, filepath.Join(t.TempDir(), "absent.env")); got != "" {
		t.Errorf("Get = %q, want empty for missing key/file", got)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
