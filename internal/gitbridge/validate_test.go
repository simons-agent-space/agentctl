package gitbridge

import (
	"strings"
	"testing"

	"github.com/simons-agent-space/agentctl/internal/policy"
)

func TestDecodeRequest_OK(t *testing.T) {
	body := `{"repo":"simons-agent-space/agentctl","profile":"builder"}`
	repo, prof, verr := decodeRequest(strings.NewReader(body))
	if verr != nil {
		t.Fatalf("decodeRequest: %v", verr)
	}
	if repo != "simons-agent-space/agentctl" {
		t.Errorf("repo = %q", repo)
	}
	if prof != policy.ProfileBuilder {
		t.Errorf("profile = %q", prof)
	}
}

func TestDecodeRequest_BadJSON(t *testing.T) {
	_, _, verr := decodeRequest(strings.NewReader("not json"))
	if verr == nil {
		t.Errorf("expected error for bad JSON")
	}
}

func TestDecodeRequest_MissingRepo(t *testing.T) {
	body := `{"profile":"builder"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil || verr.Code != "BAD_REQUEST" {
		t.Errorf("expected BAD_REQUEST, got %v", verr)
	}
}

func TestDecodeRequest_MissingProfile(t *testing.T) {
	body := `{"repo":"x/y"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil || verr.Code != "BAD_REQUEST" {
		t.Errorf("expected BAD_REQUEST, got %v", verr)
	}
}

func TestDecodeRequest_UnknownField(t *testing.T) {
	body := `{"repo":"x/y","profile":"builder","sneaky":"value"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil {
		t.Errorf("expected error for unknown field")
	}
}

func TestAuthorize_OK(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("simons-agent-space/agentctl", policy.ProfileBuilder)
	if verr != nil {
		t.Errorf("authorize: %v", verr)
	}
}

func TestAuthorize_NotBuilder(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("simons-agent-space/agentctl", "admin")
	if verr == nil || verr.Code != "PROFILE_NOT_ALLOWED" {
		t.Errorf("expected PROFILE_NOT_ALLOWED, got %v", verr)
	}
}

func TestAuthorize_RepoNotAllowed(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("simons-agent-space/other", policy.ProfileBuilder)
	if verr == nil || verr.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("expected REPO_NOT_ALLOWED, got %v", verr)
	}
}

func TestAuthorize_WrongOrg(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("other-org/agentctl", policy.ProfileBuilder)
	if verr == nil || verr.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("expected REPO_NOT_ALLOWED, got %v", verr)
	}
}
