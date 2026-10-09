package apply

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// buildInventory writes an ansible INI inventory for the host group.
// Host-key verification is mandatory: StrictHostKeyChecking=yes against
// the supplied known_hosts material — there is no path that disables it
// (same invariant as TG-10 sshexec).
func buildInventory(workDir, knownHostsFile, sshUser string, hostArgs map[string]string, hosts []string) (string, error) {
	var sb strings.Builder
	sb.WriteString("[targets]\n")
	for _, h := range hosts {
		args := hostArgs[h]
		if args != "" {
			sb.WriteString(fmt.Sprintf("%s ansible_ssh_common_args=%q\n", h, args))
		} else {
			sb.WriteString(h + "\n")
		}
	}
	sb.WriteString("[targets:vars]\n")
	sb.WriteString("ansible_user=" + sshUser + "\n")
	path := workDir + "/inventory.ini"
	if err := writeFileSecure(path, []byte(sb.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// runAnsible executes ansible-playbook with the JSON stdout callback so
// per-host results are machine-parseable. All output is streamed through
// onOutput (for recording) and stdout is also returned for parsing.
func runAnsible(ctx context.Context, ansibleBin, inventory, playbook string, checkOnly bool, extraEnv []string, onOutput func([]byte)) (string, int, error) {
	if ansibleBin == "" {
		ansibleBin = "ansible-playbook"
	}
	args := []string{"-i", inventory, playbook}
	if checkOnly {
		args = append(args, "--check")
	}
	cmd := exec.CommandContext(ctx, ansibleBin, args...)
	cmd.Env = append(cmd.Environ(),
		"ANSIBLE_STDOUT_CALLBACK=json",
		"ANSIBLE_HOST_KEY_CHECKING=True",
		"ANSIBLE_INVENTORY_UNPARSED_FAILED=True",
		"ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_FORCE_COLOR=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	var stdout bytes.Buffer
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		tee := io.TeeReader(stdoutR, &stdout)
		scan(tee, onOutput)
		scan(stderrR, onOutput)
	}()

	runErr := cmd.Run()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	<-doneCh

	exit := 0
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	} else if runErr != nil {
		exit = -1
	}
	return stdout.String(), exit, runErr
}

func scan(r io.Reader, onOutput func([]byte)) {
	br := bufio.NewReaderSize(r, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := br.Read(buf)
		if n > 0 && onOutput != nil {
			onOutput(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// parsePerHost aggregates the ansible JSON callback's per-play "stats"
// blocks into one HostResult per host. The json callback emits a list of
// play results; a multi-play playbook accumulates across plays.
func parsePerHost(stdoutJSON string) map[string]HostResult {
	per := map[string]HostResult{}
	var plays []struct {
		Stats map[string]map[string]json.Number `json:"stats"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdoutJSON)), &plays); err != nil {
		return nil
	}
	for _, play := range plays {
		for host, st := range play.Stats {
			h := per[host]
			h.OK += intNum(st["ok"])
			h.Changed += intNum(st["changed"])
			h.Failed += intNum(st["failures"])
			h.Unreachable += intNum(st["unreachable"])
			h.Skipped += intNum(st["skipped"])
			per[host] = h
		}
	}
	if len(per) == 0 {
		return nil
	}
	return per
}

func intNum(n json.Number) int {
	v, _ := n.Int64()
	return int(v)
}
