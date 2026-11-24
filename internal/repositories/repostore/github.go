package repostore

import (
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// GitHub pages for a repository are derived from its git remote on read, like
// EffectiveTags: the remote is the stored fact, and the page is a function of
// it. A remote may name its host through an SSH alias (git@gh-alias:owner/name.git,
// where ~/.ssh/config maps gh-alias to github.com), so an SSH host is resolved the
// way git itself would reach it — `ssh -G` — rather than by a table of aliases
// kept here, which would go stale the day an alias is added.

const githubHost = "github.com"

var (
	// user@host:owner/name(.git) — git's scp-like SSH form.
	scpLikeRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)
	// scheme://[user@]host[:port]/owner/name(.git)
	urlRemote = regexp.MustCompile(`^(ssh|https?|git)://(?:[^@/]+@)?([^:/]+)(?::\d+)?/([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)
)

// sshHostResolver maps an SSH host as written in a remote to the host ssh
// actually connects to. Replaced in tests.
var sshHostResolver = resolveSSHHostWithSSHConfig

var sshHostCache = struct {
	sync.Mutex
	hosts map[string]string
}{hosts: map[string]string{}}

// resolveSSHHostWithSSHConfig asks `ssh -G` for the hostname ssh would use.
// The answer is cached for the life of the process, failures included, so a
// broken alias is logged once rather than on every list read.
func resolveSSHHostWithSSHConfig(host string) (string, error) {
	sshHostCache.Lock()
	defer sshHostCache.Unlock()
	if resolved, ok := sshHostCache.hosts[host]; ok {
		if resolved == "" {
			return "", fmt.Errorf("ssh -G %s failed earlier", host)
		}
		return resolved, nil
	}
	output, err := exec.Command("ssh", "-G", host).Output()
	if err != nil {
		sshHostCache.hosts[host] = ""
		log.Printf("[repo-store] ssh -G %s: %v; repos with this remote host get no github_url", host, err)
		return "", err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if hostname, found := strings.CutPrefix(line, "hostname "); found {
			resolved := strings.TrimSpace(hostname)
			sshHostCache.hosts[host] = resolved
			return resolved, nil
		}
	}
	sshHostCache.hosts[host] = ""
	log.Printf("[repo-store] ssh -G %s printed no hostname; repos with this remote host get no github_url", host)
	return "", fmt.Errorf("ssh -G %s printed no hostname", host)
}

// GitHubURLOfRemote returns https://github.com/<owner>/<name> for a remote
// hosted on GitHub, and "" for any other remote, an empty one, or one whose
// SSH host cannot be resolved.
func GitHubURLOfRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	var host, owner, name string
	sshForm := false
	if match := urlRemote.FindStringSubmatch(remote); match != nil {
		host, owner, name = match[2], match[3], match[4]
		sshForm = match[1] == "ssh"
	} else if match := scpLikeRemote.FindStringSubmatch(remote); match != nil {
		host, owner, name = match[1], match[2], match[3]
		sshForm = true
	} else {
		return ""
	}
	if sshForm {
		resolved, err := sshHostResolver(host)
		if err != nil {
			return ""
		}
		host = resolved
	}
	if !strings.EqualFold(host, githubHost) {
		return ""
	}
	return "https://" + githubHost + "/" + owner + "/" + name
}

// RepoMentionPrefix is how a chat message names a repository so the chat can
// draw it as a reference chip: `repo:dash`. A bare name is an ordinary word
// and cannot be told apart from prose.
const RepoMentionPrefix = "repo:"

// RepoNameOfMention returns the repo name a `repo:<name>` mention carries, and
// false for anything else. The prefix is matched in any case, as the chat's
// matcher matches it.
func RepoNameOfMention(mention string) (string, bool) {
	if len(mention) <= len(RepoMentionPrefix) || !strings.EqualFold(mention[:len(RepoMentionPrefix)], RepoMentionPrefix) {
		return "", false
	}
	return mention[len(RepoMentionPrefix):], true
}
