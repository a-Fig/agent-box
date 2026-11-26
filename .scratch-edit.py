p='cmd/box/push_test.go'
s=open(p,encoding='utf-8',newline='').read()
def rep(a,b):
    global s
    assert s.count(a)==1, a
    s=s.replace(a,b)
rep('''		lines = append(lines, "FAIL line "+string(rune('a'+i%26))+strings.Repeat("x", i))
	}''','''		lines = append(lines, "FAIL line "+string(rune('a'+i%26))+strings.Repeat("x", i))
	}
	lines[25] += " GITHUB_TOKEN=ghp_abcdef123456"''')
rep('''"From box, not your user.", "Full suite for the push change", "GITHUB_TOKEN=*** go test ./...  in  " + e.repo, lines[29], "Full log: box_job id " + job + " offset 0. Artifacts: box_pull, before this repo's next run."}''','''"From box, not your user.", "GITHUB_TOKEN=***", lines[29], "Full result: box_job id " + job + ". Artifacts: box_pull, before this repo's next run."}''')
rep('''	if strings.Contains(got.msg, "ghp_abcdef123456") || strings.Contains(got.msg, lines[9]) || !strings.Contains(got.msg, lines[10]) {
		t.Errorf("secret kept, or the tail isn't the last 20 lines:\n%s", got.msg)
	}''','''	if strings.Contains(got.msg, "ghp_abcdef123456") || strings.Contains(got.msg, lines[19]+"\n") || !strings.Contains(got.msg, lines[20]) {
		t.Errorf("secret kept, or the tail isn't the last 10 lines:\n%s", got.msg)
	}
	// The reason and command aren't repeated: the agent has them from box_run.
	if strings.Contains(got.msg, "Full suite for the push change") || strings.Contains(got.msg, "go test ./...") {
		t.Errorf("message repeats the run's reason or command:\n%s", got.msg)
	}''')
rep('''	if !strings.Contains(e.push.sent[0].msg, "Box and log kept up to 30 minutes") {''','''	if !strings.Contains(e.push.sent[0].msg, "box and log kept up to 30 minutes") {''')
rep('''	if msg := e.push.sent[0].msg; !strings.Contains(msg, "\n"+want+"Output tail:\nFAIL\tp\t0.1s\n") {
		t.Fatalf("pushed message:\n%s", msg)
	}''','''	// A test failure is pushed as its digest, without the output tail.
	if msg := e.push.sent[0].msg; !strings.Contains(msg, "not your user.\n"+want+"Full result: box_job id "+job+".") || strings.Contains(msg, "FAIL\tp\t0.1s") {
		t.Fatalf("pushed message:\n%s", msg)
	}''')
rep('''func TestBoxJobAlwaysReturnsTheFullResult(''','''// A pass is pushed as one line: its digest, how long, and where the full
// result is (#182).
func TestPassIsPushedAsOneLine(t *testing.T) {
	r := jobRecord{Job: "my-repo.20261123-120000-0001", Reason: "Why.", Command: "go test ./...", Worktree: "w"}
	end := jobEnd{Took: 372, Tail: "ok  p  0.1s\n", Digest: "PASS: 2325 tests passed, 28 skipped (go test)\nPASS: 4 tests passed (vitest)\n"}
	want := "box: job my-repo.20261123-120000-0001 ended, exit 0, after 6m12s: PASS: 2325 tests passed, 28 skipped (go test); PASS: 4 tests passed (vitest). From box, not your user. Full result: box_job id my-repo.20261123-120000-0001. Artifacts: box_pull, before this repo's next run."
	if got := endedMessage(r, end); got != want {
		t.Errorf("message =\n%s\nwant\n%s", got, want)
	}
	end.Digest = ""
	if got := endedMessage(r, end); strings.Contains(got, "\n") || strings.Contains(got, "ok  p") || !strings.HasPrefix(got, "box: job my-repo.20261123-120000-0001 ended, exit 0, after 6m12s. From box") {
		t.Errorf("pass without a digest = %q", got)
	}
	// Exit 0 with failed tests (go test ... || true) still shows the failures.
	end.Digest = "FAIL: 1 of 2 tests failed (go test)\np\n  TestQ  q_test.go:8: no\n"
	if got := endedMessage(r, end); !strings.Contains(got, "\n  TestQ  q_test.go:8: no\n") {
		t.Errorf("exit 0 with failed tests = %q", got)
	}
}

// A failure whose digest reports no failed tests (the tests passed, a later
// step failed) gets the digest and the tail.
func TestFailureWithPassingDigestKeepsTheTail(t *testing.T) {
	r := jobRecord{Job: "my-repo.20261123-120000-0001"}
	got := endedMessage(r, jobEnd{Code: 2, Tail: "lint: bad.go:3\n", Digest: "PASS: 3 tests passed (go test)\n"})
	if !strings.Contains(got, "\nPASS: 3 tests passed (go test)\nlint: bad.go:3\nFull result:") {
		t.Errorf("message = %q", got)
	}
}

func TestBoxJobAlwaysReturnsTheFullResult(''')
open(p,'w',encoding='utf-8',newline='').write(s)
