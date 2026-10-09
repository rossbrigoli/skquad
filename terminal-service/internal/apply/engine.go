package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultApplyTimeout bounds a full playbook run.
const DefaultApplyTimeout = 15 * time.Minute

// Apply runs the full §6.7 pipeline: verify approval → lint → run with
// recording. Every refusal is terminal (nothing partially applied); every
// outcome carries lint findings for audit.
func (e *Engine) Apply(ctx context.Context, req Request) (*Result, error) {
	started := time.Now()
	res := &Result{Status: StatusRunning, StartedAt: started}

	if len(req.Hosts) == 0 {
		res.Status = StatusRefused
		res.RefusalReason = RefusalEmptyHosts
		res.FinishedAt = time.Now()
		return res, nil
	}
	if req.GitURL == "" || req.DefaultBranch == "" {
		return nil, errors.New("apply: GitURL and DefaultBranch are required")
	}
	if req.Timeout <= 0 {
		req.Timeout = DefaultApplyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	workDir, err := os.MkdirTemp(e.WorkRoot, "skquad-apply-")
	if err != nil {
		return nil, fmt.Errorf("apply: workdir: %w", err)
	}
	defer func() {
		// Wipe clone + certs + inventory. Recording lives in object
		// storage, not here.
		_ = os.RemoveAll(workDir)
	}()

	// 1. Approval binding: merged revision.
	fullRev, refusal, err := gitVerify(ctx, e.GitBin, req.GitURL, req.GitRev, req.DefaultBranch, workDir, req.RequireTip)
	if err != nil {
		return nil, fmt.Errorf("apply: git verify: %w", err)
	}
	if refusal != "" {
		res.Status = StatusRefused
		res.RefusalReason = refusal
		res.FinishedAt = time.Now()
		return res, nil
	}
	res.RecordingID = req.ApplyID

	// 2. Playbook path safety: must live inside playbooks_path.
	playbooksRoot := filepath.Clean(filepath.Join(workDir, "repo", req.PlaybooksPath))
	playbookPath := filepath.Clean(filepath.Join(playbooksRoot, req.Playbook))
	if !strings.HasPrefix(playbookPath, playbooksRoot+string(os.PathSeparator)) {
		res.Status = StatusRefused
		res.RefusalReason = RefusalBadPlaybook
		res.FinishedAt = time.Now()
		return res, nil
	}
	pbBytes, err := os.ReadFile(playbookPath)
	if err != nil {
		res.Status = StatusRefused
		res.RefusalReason = RefusalBadPlaybook
		res.FinishedAt = time.Now()
		return res, nil
	}

	// 3. Lint gate.
	findings, err := LintPlaybook(pbBytes, req.MirrorsAllow, req.DenyPatterns)
	if err != nil {
		return nil, fmt.Errorf("apply: lint: %w", err)
	}
	res.LintFindings = findings
	for _, f := range findings {
		if f.Fatal {
			res.Status = StatusRefused
			res.RefusalReason = RefusalLintFailed
			res.FinishedAt = time.Now()
			return res, nil
		}
	}

	// 4. Recorder (failure to create is not fatal — recording is
	//    best-effort by design, same as TG-10).
	var rec Recorder
	if e.NewRecorder != nil {
		rec, _ = e.NewRecorder(req.ApplyID, map[string]any{
			"kind": "artifact_apply", "apply_id": req.ApplyID, "resource_id": req.ResourceID,
			"agent_id": req.AgentID, "playbook": req.Playbook, "git_rev": fullRev,
			"host_group": req.HostGroup, "check_only": req.CheckOnly, "hosts": req.Hosts,
		})
	}
	record := func(b []byte) {
		if rec != nil {
			_ = rec.AppendOut(b)
		}
	}

	// 5. SSH material: per-host CA certs (or one static key).
	knownHostsFile := workDir + "/known_hosts"
	if err := writeFileSecure(knownHostsFile, []byte(req.KnownHosts+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("apply: known_hosts: %w", err)
	}
	hostArgs := map[string]string{}
	baseArgs := "-o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + knownHostsFile
	for _, h := range req.Hosts {
		if req.CAMint != nil {
			certPEM, err := req.CAMint.Mint(ctx, req.SSHUser, h, req.CertTTL)
			if err != nil {
				res.Status = StatusRefused
				res.RefusalReason = RefusalCertMint
				res.FinishedAt = time.Now()
				return res, nil
			}
			certFile := filepath.Join(workDir, "cert-"+sanitize(h)+".pem")
			if err := writeFileSecure(certFile, certPEM, 0o600); err != nil {
				return nil, fmt.Errorf("apply: cert write: %w", err)
			}
			keyFile := filepath.Join(workDir, "key-"+sanitize(h)+".pem")
			if err := writeFileSecure(keyFile, []byte(req.StaticKeyPEM), 0o600); err != nil {
				return nil, fmt.Errorf("apply: key write: %w", err)
			}
			hostArgs[h] = baseArgs + " -o CertificateFile=" + certFile + " -o IdentityFile=" + keyFile + " -o IdentitiesOnly=yes"
		} else if req.StaticKeyPEM != "" {
			keyFile := filepath.Join(workDir, "static-key.pem")
			if err := writeFileSecure(keyFile, []byte(req.StaticKeyPEM), 0o600); err != nil {
				return nil, fmt.Errorf("apply: key write: %w", err)
			}
			hostArgs[h] = baseArgs + " -o IdentityFile=" + keyFile + " -o IdentitiesOnly=yes"
		} else {
			return nil, errors.New("apply: no SSH auth material (CAMint or StaticKeyPEM required)")
		}
	}

	// 6. Run ansible.
	inventory, err := buildInventory(workDir, knownHostsFile, req.SSHUser, hostArgs, req.Hosts)
	if err != nil {
		return nil, fmt.Errorf("apply: inventory: %w", err)
	}
	stdout, exitCode, _ := runAnsible(ctx, e.AnsibleBin, inventory, playbookPath, req.CheckOnly, nil, record)

	if rec != nil {
		_ = rec.Flush(ctx)
		_ = rec.Close(context.Background())
	}

	res.ExitCode = exitCode
	if per := parsePerHost(stdout); per != nil {
		res.PerHost = per
	}
	if exitCode == 0 {
		res.Status = StatusSucceeded
	} else {
		res.Status = StatusFailed
	}
	res.FinishedAt = time.Now()
	return res, nil
}

func writeFileSecure(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Chmod(mode)
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(s)
}
