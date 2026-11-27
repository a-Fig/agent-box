package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func pullTool(a *App, from, remote, local string) map[string]any {
	params, _ := json.Marshal(map[string]any{"name": "box_pull", "arguments": map[string]string{
		"from": from, "remotePath": remote, "localDir": local,
	}})
	return callTool(context.Background(), a, params, nil)
}

func TestQualifiedPullRoutesSameBareID(t *testing.T) {
	a, _, west, east := lookupTestApp(t, "work")
	westID := requireQualifiedJobID(t, west, testBareJobID).ID
	eastID := requireQualifiedJobID(t, east, testBareJobID).ID
	for _, tc := range []struct {
		name, id, bare string
		host           fleetHost
		mcp            bool
	}{
		{"qualified CLI", westID, testBareJobID, west, false},
		{"qualified MCP", eastID, testBareJobID, east, true},
		{"legacy job ID", testBareJobID, testBareJobID, east, false},
		{"legacy box name", "repo", "", east, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.host.Remote + ":repo"
			checkout := []string{"--project", tc.host.Project, "exec", target, "--", "bash", "-c"}
			transfer := []string{"--project", tc.host.Project, "file", "pull", "-r", target + workRoot + "/checkout/result.txt"}
			var calls []string
			a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
				switch {
				case slices.Equal(cmd.Args, append(slices.Clone(checkout), checkoutScript(tc.bare))):
					calls = append(calls, "checkout")
					return Result{Stdout: workRoot + "/checkout\n"}, nil
				case tc.bare != "" && slices.Equal(cmd.Args, append(slices.Clone(checkout), checkoutOwnerScript(tc.bare, "checkout"))):
					calls = append(calls, "owner")
					return Result{Stdout: "checkout\n"}, nil
				case len(cmd.Args) == 7 && slices.Equal(cmd.Args[:6], transfer):
					calls = append(calls, "transfer")
					write(t, filepath.Join(cmd.Args[6], "result.txt"), tc.host.Name+" artifact")
					return Result{}, nil
				default:
					t.Fatalf("unexpected Incus pull request: %q", cmd.Args)
					return Result{}, nil
				}
			})
			local := t.TempDir()
			if tc.mcp {
				if res := pullTool(a, tc.id, "result.txt", local); res["isError"] != false {
					t.Fatalf("box_pull = %v", res)
				}
			} else if code := run(context.Background(), a, []string{"pull", tc.id, "result.txt", local}, nil); code != 0 {
				t.Fatalf("box pull exited %d", code)
			}
			data, err := os.ReadFile(filepath.Join(local, "result.txt"))
			if err != nil || string(data) != tc.host.Name+" artifact" {
				t.Fatalf("artifact = %q, %v", data, err)
			}
			want := []string{"checkout", "transfer"}
			if tc.bare != "" {
				want = append(want, "owner")
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("pull calls = %v, want %v", calls, want)
			}
		})
	}
}

func TestQualifiedFrozenPullRejectsReplacementDuringTransfer(t *testing.T) {
	a, _, west, _ := lookupTestApp(t, "work")
	id := requireQualifiedJobID(t, west, testBareJobID).ID
	target := west.Remote + ":repo"
	owner := testBareJobID
	const nextOwner = "repo.20261126-224000-4abc"
	ownerReads := 0
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if len(cmd.Args) < 5 || cmd.Args[1] != west.Project {
			t.Fatalf("wrong Incus project: %q", cmd.Args)
		}
		if len(cmd.Args) == 8 && cmd.Args[2] == "exec" && cmd.Args[3] == target &&
			(cmd.Args[7] == checkoutScript(testBareJobID) || cmd.Args[7] == checkoutOwnerScript(testBareJobID, "checkout")) {
			return Result{Code: 1, Stderr: "Instance is frozen"}, nil
		}
		switch cmd.Args[4] {
		case target + workRoot + "/.box/" + testBareJobID + ".repo":
			return Result{Stdout: "checkout\n"}, nil
		case target + workRoot + "/.box/checkout.checkout":
			ownerReads++
			return Result{Stdout: owner + "\n"}, nil
		case "-r":
			if len(cmd.Args) != 7 || cmd.Args[5] != target+workRoot+"/checkout/result.txt" || ownerReads != 1 {
				t.Fatalf("unexpected transfer before owner check: %q, reads %d", cmd.Args, ownerReads)
			}
			write(t, filepath.Join(cmd.Args[6], "result.txt"), "replacement data")
			owner = nextOwner
			return Result{}, nil
		default:
			t.Fatalf("unexpected frozen-box pull request: %q", cmd.Args)
			return Result{}, nil
		}
	})
	local := t.TempDir()
	if _, err := a.Pull(context.Background(), id, "result.txt", local); err == nil || !strings.Contains(err.Error(), "was replaced by job "+nextOwner) || ownerReads != 2 {
		t.Fatalf("pull across checkout replacement = %v, owner reads = %d", err, ownerReads)
	}
	if entries, err := os.ReadDir(local); err != nil || len(entries) != 0 {
		t.Fatalf("aborted pull left files or staging: %v, %v", entries, err)
	}
}

func TestQualifiedPullRejectsUnknownHostWithoutIO(t *testing.T) {
	a, _, _, _ := lookupTestApp(t, "work")
	id := requireQualifiedJobID(t, fleetHost{Remote: "missing", Project: "agents"}, testBareJobID).ID
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		t.Fatalf("unknown host called Incus: %q", cmd.Args)
		return Result{}, nil
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unknown host called boxd: %s %s", req.Method, req.URL)
		return nil, nil
	}
	local := filepath.Join(t.TempDir(), "not-created")
	if res := pullTool(a, id, "result.txt", local); res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "not configured") {
		t.Fatalf("unknown host MCP result = %v", res)
	}
	if code := run(context.Background(), a, []string{"pull", id, "result.txt", local}, nil); code == 0 {
		t.Fatal("CLI accepted an unknown pull target")
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("unknown host touched the destination: %v", err)
	}
}
