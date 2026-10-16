package interactive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/operator"
)

const testImage = "registry.invalid/market@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func input(lines []string) io.Reader { return strings.NewReader(strings.Join(lines, "\n") + "\n") }
func development(dir string) []string {
	return []string{"1", "dev", dir, "default", testImage, "clearnet", "127.0.0.1:8080", "", "public", "open", "", "", "", "", "", "", "n"}
}
func fakeRunners(calls *[][]string) runners {
	return runners{
		deploy: func(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
			*calls = append(*calls, append([]string(nil), args...))
			return nil
		},
		keys: func(_ context.Context, args []string, _ io.Writer) error {
			*calls = append(*calls, append([]string{"keys"}, args...))
			return nil
		},
	}
}
func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestCancelAndEOFCannotWriteOrStart(t *testing.T) {
	dir := filepath.Join(testDir(t), "new")
	for name, lines := range map[string][]string{
		"quit": {"quit"}, "early EOF": {"1", "dev"}, "review EOF": development(dir),
		"default no": append(development(dir), ""), "explicit no": append(development(dir), "n"),
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			var out, errOut bytes.Buffer
			err := run(context.Background(), input(lines), &out, &errOut, fakeRunners(&calls))
			if err != nil && !errors.Is(err, ErrCancelled) {
				t.Fatal(err)
			}
			if len(calls) != 0 {
				t.Fatalf("cancel invoked command: %#v", calls)
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatal("cancel created state")
			}
		})
	}
}

func TestCreateUsesOnlyReviewedFlagsThenPlan(t *testing.T) {
	dir := filepath.Join(testDir(t), "new")
	var calls [][]string
	var out, errOut bytes.Buffer
	answers := append(development(dir), "y", "n")
	if err := run(context.Background(), input(answers), &out, &errOut, fakeRunners(&calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != "init" || calls[1][0] != "plan" {
		t.Fatalf("unexpected commands: %#v", calls)
	}
	for key, want := range map[string]string{"mode": "dev", "dir": dir, "docker-context": "default", "app-image": testImage, "exposure": "clearnet", "site-access": "public", "registrations": "open"} {
		if got := flagValue(calls[0], key); got != want {
			t.Fatalf("%s=%q, want%q", key, got, want)
		}
	}
	if !strings.Contains(out.String(), "Back up operator.json") || !strings.Contains(out.String(), "Equivalent") && !strings.Contains(out.String(), "equivalent command") {
		t.Fatal("missing review/backup guidance")
	}
}

func TestReviewEditKeepsDefaultsAndReplacesOnlySelectedAnswer(t *testing.T) {
	dir := filepath.Join(testDir(t), "new")
	other := strings.Replace(testImage, "aaaa", "bbbb", 1)
	answers := append(development(dir), "edit")
	second := make([]string, 16)
	second[3] = other // mode, dir, context, app image
	answers = append(answers, second...)
	answers = append(answers, "y", "n")
	var calls [][]string
	if err := run(context.Background(), input(answers), io.Discard, io.Discard, fakeRunners(&calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || flagValue(calls[0], "app-image") != other || flagValue(calls[0], "dir") != dir {
		t.Fatalf("edited command: %#v", calls)
	}
}

func TestReviewEditCanClearAnOptionalImport(t *testing.T) {
	base := testDir(t)
	source := filepath.Join(base, "existing-app-key")
	if err := os.WriteFile(source, []byte("PRIVATE-CONTENTS-MUST-NOT-PRINT"), 0600); err != nil {
		t.Fatal(err)
	}
	answers := development(filepath.Join(base, "new"))
	answers[13] = source
	answers = append(answers, "edit")
	second := make([]string, 16)
	second[12] = ":clear"
	answers = append(answers, second...)
	answers = append(answers, "y", "n")
	var calls [][]string
	var out bytes.Buffer
	if err := run(context.Background(), input(answers), &out, io.Discard, fakeRunners(&calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || flagValue(calls[0], "app-secret-key-file") != "" {
		t.Fatalf("import was not cleared: %#v", calls)
	}
	if !strings.Contains(out.String(), "Generated application key:") || strings.Contains(out.String(), "PRIVATE-CONTENTS-MUST-NOT-PRINT") {
		t.Fatal("incorrect material summary")
	}
}

func TestPromptRejectsControlCharactersWithoutEchoingThem(t *testing.T) {
	for _, control := range []string{"\x1b[31m", "\a", "\t", "\x00", "\x7f"} {
		t.Run(strconv.Quote(control), func(t *testing.T) {
			var out, errOut bytes.Buffer
			w := &wizard{ctx: context.Background(), out: &out, errOut: &errOut, answers: map[string]string{}, lines: make(chan inputLine, 2)}
			w.lines <- inputLine{text: "unsafe" + control}
			w.lines <- inputLine{text: "safe"}
			value, err := w.ask("test", "Value", "", "help", required)
			if err != nil || value != "safe" || !strings.Contains(errOut.String(), "control characters") {
				t.Fatalf("%q: %v %s", value, err, errOut.String())
			}
			if strings.Contains(out.String()+errOut.String(), control) {
				t.Fatal("control input echoed")
			}
		})
	}
}

func TestRequiredAnswerCannotBeCleared(t *testing.T) {
	var out, errOut bytes.Buffer
	w := &wizard{ctx: context.Background(), out: &out, errOut: &errOut, answers: map[string]string{"required": "retained"}, lines: make(chan inputLine, 2)}
	w.lines <- inputLine{text: ":clear"}
	w.lines <- inputLine{text: ""}
	value, err := w.ask("required", "Required value", "", "help", required)
	if err != nil || value != "retained" || !strings.Contains(errOut.String(), "cannot be cleared") {
		t.Fatalf("%q %v %s", value, err, errOut.String())
	}
}

func TestOptionalInstallRequiresSecondConfirmation(t *testing.T) {
	for _, answer := range []string{"n", "y"} {
		t.Run(answer, func(t *testing.T) {
			var calls [][]string
			answers := append(development(filepath.Join(testDir(t), "new")), "y", answer)
			if err := run(context.Background(), input(answers), io.Discard, io.Discard, fakeRunners(&calls)); err != nil {
				t.Fatal(err)
			}
			want := 2
			if answer == "y" {
				want = 3
			}
			if len(calls) != want {
				t.Fatalf("calls %#v", calls)
			}
			if answer == "y" && calls[2][0] != "install" {
				t.Fatal(calls)
			}
		})
	}
}

func TestProductionCollectsPathsWithoutPrintingSecretContents(t *testing.T) {
	base := testDir(t)
	secret := filepath.Join(base, "private-input")
	marker := "postgres://operator:THIS-MUST-NOT-PRINT@db.internal/market?sslmode=verify-full"
	if err := os.WriteFile(secret, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "new")
	answers := []string{"1", "prod", dir, "default", testImage, "clearnet", "10.20.0.10:8080", filepath.Join(base, "retention"), "members", "closed", secret, "", "app.internal", "https://market.example", "10.20.0.10:8443", secret, secret, secret, secret, secret, "", "", "", "n", "y", "n"}
	var calls [][]string
	var out, errOut bytes.Buffer
	if err := run(context.Background(), input(answers), &out, &errOut, fakeRunners(&calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || flagValue(calls[0], "mode") != "prod" || flagValue(calls[0], "database-url-file") != secret || flagValue(calls[0], "upload-key") != secret {
		t.Fatalf("production shape: %#v", calls)
	}
	if strings.Contains(out.String()+errOut.String(), marker) || strings.Contains(out.String()+errOut.String(), "THIS-MUST-NOT-PRINT") {
		t.Fatal("secret printed")
	}
	if flagValue(calls[0], "clamav-image") != "" || flagValue(calls[0], "postgres-image") != "" {
		t.Fatal("production added local workers")
	}
}

func TestOnionInputHelpValidationAndExplicitWrite(t *testing.T) {
	dir := filepath.Join(testDir(t), "identity")
	answers := []string{"2", "1", dir, "UPPERCASE", "?", "a", "100", "1", "y"}
	var calls [][]string
	var out, errOut bytes.Buffer
	if err := run(context.Background(), input(answers), &out, &errOut, fakeRunners(&calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || strings.Join(calls[0][:3], " ") != "keys generate onion" || flagValue(calls[0], "prefix") != "a" || flagValue(calls[0], "workers") != "1" {
		t.Fatalf("onion command: %#v", calls)
	}
	if !strings.Contains(errOut.String(), "lowercase") || !strings.Contains(out.String(), "1,073,741,824") {
		t.Fatal("missing validation/help")
	}
	if !strings.Contains(out.String(), "Searching locally for a matching prefix") || !strings.Contains(out.String(), "no onion service is published") {
		t.Fatal("missing visible search/cancellation notice")
	}
}

func TestDeploymentPrefixWarnsBeforeRunner(t *testing.T) {
	answers := development(filepath.Join(testDir(t), "new"))
	answers[5] = "tor"
	answers = append(answers, testImage, "1", "a", "n", "y", "n")
	var out bytes.Buffer
	var calls [][]string
	execute := fakeRunners(&calls)
	original := execute.deploy
	execute.deploy = func(ctx context.Context, args []string, in io.Reader, stdout, stderr io.Writer) error {
		if args[0] == "init" && !strings.Contains(out.String(), "Searching locally for a matching prefix") {
			t.Fatal("search notice was hidden until after generation")
		}
		return original(ctx, args, in, stdout, stderr)
	}
	if err := run(context.Background(), input(answers), &out, io.Discard, execute); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || flagValue(calls[0], "onion-prefix") != "a" {
		t.Fatal(calls)
	}
}

func TestManagePlanDefaultAndLifecycleCancel(t *testing.T) {
	for _, action := range []string{"", "down", "up", "redeploy"} {
		t.Run(action, func(t *testing.T) {
			lines := []string{"3", testDir(t), action}
			if action == "redeploy" {
				lines = append(lines, testImage)
			}
			if action != "" {
				lines = append(lines, "n")
			}
			var calls [][]string
			if err := run(context.Background(), input(lines), io.Discard, io.Discard, fakeRunners(&calls)); err != nil {
				t.Fatal(err)
			}
			if action == "" {
				if len(calls) != 1 || calls[0][0] != "plan" {
					t.Fatal(calls)
				}
			} else if len(calls) != 0 {
				t.Fatal("unconfirmed lifecycle ran", calls)
			}
		})
	}
}

func TestCancelledContextDoesNotWaitForBlockedInput(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var calls [][]string
	go func() { done <- run(ctx, r, io.Discard, io.Discard, fakeRunners(&calls)) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked on terminal input")
	}
	if len(calls) != 0 {
		t.Fatal(calls)
	}
}

func TestRealDevelopmentSaveDoesNotInvokeDocker(t *testing.T) {
	base := testDir(t)
	dir := filepath.Join(base, "new")
	t.Setenv("PATH", base)
	var calls [][]string
	execute := runners{deploy: func(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		if args[0] != "init" && args[0] != "plan" {
			t.Fatal("unexpected Docker action", args)
		}
		return operator.Run(ctx, args, in, out, errOut)
	}, keys: func(context.Context, []string, io.Writer) error { t.Fatal("unexpected key action"); return nil }}
	if err := run(context.Background(), input(append(development(dir), "y", "n")), io.Discard, io.Discard, execute); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatal(calls)
	}
	for _, name := range []string{"operator.json", "secrets/app-secret-key", "secrets/setup-token"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatal("unsafe file permissions", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.json")); !os.IsNotExist(err) {
		t.Fatal("wizard installed runtime without permission")
	}
}

func TestGuidedInitialAccessChoicesAreValidatedAndReviewed(t *testing.T) {
	answers := development(filepath.Join(testDir(t), "new"))
	answers[8], answers[9] = "members", "closed"
	// Reject an unsupported policy before accepting the selected members setting.
	answers = append(append(append([]string{}, answers[:8]...), "private"), answers[8:]...)
	answers = append(answers, "y", "n")
	var calls [][]string
	var out, errOut bytes.Buffer
	if err := run(context.Background(), input(answers), &out, &errOut, fakeRunners(&calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || flagValue(calls[0], "site-access") != "members" || flagValue(calls[0], "registrations") != "closed" {
		t.Fatalf("access choices not saved: %#v", calls)
	}
	if !strings.Contains(errOut.String(), "Choose one of the displayed values.") || !strings.Contains(out.String(), "'--site-access' 'members'") || !strings.Contains(out.String(), "'--registrations' 'closed'") {
		t.Fatalf("missing validation or review: %s %s", out.String(), errOut.String())
	}
}
