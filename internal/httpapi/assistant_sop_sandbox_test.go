package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func TestSOPHelperResolvesBuiltInExecutorCredentials(t *testing.T) {
	for _, tc := range []struct {
		name        string
		executor    string
		credentials repo.UserCredentialUpdate
		wantPrompt  bool
	}{
		{"codebuddy", "codebuddy", repo.UserCredentialUpdate{CodeBuddyAPIKey: "selector-test-key"}, true},
		{"codex", "codex", repo.UserCredentialUpdate{CodexAPIKey: "selector-test-key"}, true},
		{"claude-id", "claude", repo.UserCredentialUpdate{ClaudeCodeAPIKey: "selector-test-key"}, true},
		{"claude-kind", "claude-code", repo.UserCredentialUpdate{ClaudeCodeAPIKey: "selector-test-key"}, true},
		{"missing-key", "claude", repo.UserCredentialUpdate{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := repo.OpenTest()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(store.Close)
			if _, err := store.UpdateUserCredentials(1, tc.credentials); err != nil {
				t.Fatal(err)
			}
			h := &assistantHandlers{deps: &deps{repo: store}}
			promptCalled := false
			_, err = h.runSOPHelper(context.Background(), domain.TelemetryContext{ResponsibleUserID: 1}, domain.AssistantDefaults{Executor: tc.executor}, "", func(user domain.User) string {
				promptCalled = true
				// An empty prompt stops before launching a CLI, after the real
				// credential and runner checks have accepted the selected user.
				return ""
			})
			if promptCalled != tc.wantPrompt || errors.Is(err, errSOPHelperCredentials) == tc.wantPrompt {
				t.Fatalf("executor %q: prompt called = %v, error = %v", tc.executor, promptCalled, err)
			}
		})
	}
}

func TestAutomaticSOPSandboxUsesEachProjectsToolchain(t *testing.T) {
	for _, executor := range []string{"codebuddy", "claude"} {
		t.Run(executor, func(t *testing.T) {
			testAutomaticSOPSandboxUsesEachProjectsToolchain(t, executor)
		})
	}
}

func testAutomaticSOPSandboxUsesEachProjectsToolchain(t *testing.T, executor string) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	const key = "selector-test-model-key"
	credentials := repo.UserCredentialUpdate{CodeBuddyAPIKey: key}
	if executor == "claude" {
		credentials = repo.UserCredentialUpdate{ClaudeCodeAPIKey: key}
	}
	if _, err := store.UpdateUserCredentials(1, credentials); err != nil {
		t.Fatal(err)
	}
	projects := []struct{ id, name, toolchain, image string }{
		{"python-app", "Python Project", "Python 3.12 and uv", "python:3.12"},
		{"java-app", "Java Project", "JDK 21 and Maven", "eclipse-temurin:21"},
	}
	for _, project := range projects {
		if err := store.PutProject(domain.Project{ID: project.id, Key: project.id, Name: project.name, OwnerID: 1, Description: project.toolchain}); err != nil {
			t.Fatal(err)
		}
		if err := store.PutSandbox(domain.SandboxProfile{
			ID: project.id + "-box", Name: project.name, ProjectID: project.id, Driver: domain.SandboxDriverDocker,
			Toolchain: project.toolchain, Dockerfile: &domain.SandboxDockerfile{Content: "FROM " + project.image},
			Environment: map[string]string{"TEST_TOKEN": "environment-value-must-stay-private"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, executor)
	selector := `#!/bin/sh
for arg in "$@"; do input=$arg; done
if [ "$input" = -- ]; then input=$(cat); fi
case "$input" in
  *environment-value-must-stay-private*|*selector-test-model-key*|*'FROM '*) exit 2 ;;
  *python-app-box*java-app-box*|*java-app-box*python-app-box*) exit 3 ;;
  *python-app-box*'Python 3.12 and uv'*) id=python-app-box ;;
  *java-app-box*'JDK 21 and Maven'*) id=java-app-box ;;
  *) exit 4 ;;
esac
printf '{"type":"result","result":"{\\"sandboxProfileId\\":\\"%s\\"}"}\n' "$id"
`
	if err := os.WriteFile(bin, []byte(selector), 0o700); err != nil {
		t.Fatal(err)
	}
	if executor == "claude" {
		t.Setenv("AGENTFLOW_CLAUDE_BIN", bin)
	} else {
		t.Setenv("AGENTFLOW_CODEBUDDY_BIN", bin)
	}
	e := engine.New(store, runtime.NewManager(filepath.Join(dir, "workspace")))
	e.ConfigureExecution("api-only", "test", 30*time.Second, false)
	api := New(store, e, tracker.NewService(), "", "")
	wf := requestJSON[domain.WorkflowDefinition](t, api, http.MethodPut, "/api/workflows", domain.WorkflowDefinition{
		ID: "shared-auto", Name: "共享验证流程", ProjectID: "platform", Shared: true, AutoSelectSandbox: true,
		Nodes: []domain.WorkflowNode{{ID: "check", Name: "Check", Type: "script", ScriptLanguage: "shell", Script: "true"}},
	}, http.StatusOK)
	if !wf.AutoSelectSandbox || wf.SandboxProfileID != "" {
		t.Fatalf("automatic workflow was saved with a fixed profile: %+v", wf)
	}
	for _, project := range projects {
		want := project.id + "-box"
		var run domain.Run
		if project.id == "python-app" {
			run = requestJSON[domain.Run](t, api, http.MethodPost, "/api/runs", map[string]string{
				"workflowId": wf.ID, "projectId": project.id, "input": "Validate toolchain. " + key,
			}, http.StatusCreated)
		} else {
			issue := domain.Issue{ID: "auto-sandbox-issue", ProjectID: project.id, Title: "Validate toolchain", Description: key, WorkflowID: wf.ID, AssigneeUserID: 1}
			if err := store.PutIssue(issue); err != nil {
				t.Fatal(err)
			}
			task := requestJSON[domain.Task](t, api, http.MethodPost, "/api/issues/"+issue.ID+"/dispatch", map[string]string{}, http.StatusCreated)
			run.ID = task.RunID
		}
		run = requestJSON[domain.Run](t, api, http.MethodGet, "/api/runs/"+run.ID, nil, http.StatusOK)
		if run.WorkflowSnapshot.SandboxProfileID != want || run.WorkflowSnapshot.SandboxSnapshot == nil || run.WorkflowSnapshot.SandboxSnapshot.Dockerfile == nil ||
			run.WorkflowSnapshot.SandboxSnapshot.Dockerfile.Content != "FROM "+project.image {
			t.Fatalf("project %s selected %q, want %q", project.id, run.WorkflowSnapshot.SandboxProfileID, want)
		}
		if len(run.Events) != 1 || run.Events[0].Type != domain.RunEventSandboxSelected || run.Events[0].Message != "Agent 选择沙箱 "+want {
			t.Fatalf("run detail events = %+v", run.Events)
		}
		stream, ok := store.RunStreamView(run.ID)
		if !ok {
			t.Fatal("run stream view missing")
		}
		events := publicLiveRun(stream).Events
		if len(events) != 1 || events[0] != run.Events[0] {
			t.Fatalf("run stream lost the sandbox selection: %+v", events)
		}
	}
}
