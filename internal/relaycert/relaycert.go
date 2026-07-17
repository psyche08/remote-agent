// Package relaycert discovers the private-tunnel client certificate a device
// should present to the relay. Shared by log upload and auto-update; the
// lookup order is a superset of the historical logupload behavior so existing
// deployments keep resolving the same pair.
package relaycert

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Discover loads the client keypair. Explicit certFile/keyFile win; otherwise
// the pair is discovered under certDir via DiscoverPair.
func Discover(certDir, certFile, keyFile, userID, deviceID string) (tls.Certificate, error) {
	certFile = expandUser(certFile)
	keyFile = expandUser(keyFile)
	if certFile == "" || keyFile == "" {
		var err error
		certFile, keyFile, err = DiscoverPair(expandUser(certDir), userID, deviceID)
		if err != nil {
			return tls.Certificate{}, err
		}
	}
	return tls.LoadX509KeyPair(certFile, keyFile)
}

// DiscoverPair finds a .crt/.key pair under dir. Preference order:
// user-<user> > agent-<user>-<device> > <user>-agent > agent-<user> (when the
// user id is known); then agent-*-<device> (upgrading to that agent's user
// cert when present); then user-*, agent-*, and finally the legacy *-agent
// naming (e.g. user-agent.crt) so devices work without a configured user id.
func DiscoverPair(dir string, userID string, deviceID string) (string, string, error) {
	if userID != "" {
		prefixes := []string{"user-" + userID}
		if deviceID != "" {
			prefixes = append(prefixes, "agent-"+userID+"-"+deviceID)
		}
		prefixes = append(prefixes, userID+"-agent", "agent-"+userID)
		for _, prefix := range prefixes {
			if cert, key, ok := existingPair(filepath.Join(dir, prefix)); ok {
				return cert, key, nil
			}
		}
	}
	if deviceID != "" {
		if cert, key, user, ok := agentPairForDevice(dir, deviceID); ok {
			if cert2, key2, ok := existingPair(filepath.Join(dir, "user-"+user)); ok {
				return cert2, key2, nil
			}
			return cert, key, nil
		}
	}
	for _, pattern := range []string{"user-*.crt", "agent-*.crt", "*-agent.crt"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return "", "", err
		}
		sort.Strings(matches)
		for _, cert := range matches {
			key := strings.TrimSuffix(cert, ".crt") + ".key"
			if st, err := os.Stat(key); err == nil && !st.IsDir() {
				return cert, key, nil
			}
		}
	}
	return "", "", fmt.Errorf("no user or agent cert/key pair under %s", dir)
}

func existingPair(prefix string) (string, string, bool) {
	cert := prefix + ".crt"
	key := prefix + ".key"
	if st, err := os.Stat(cert); err == nil && !st.IsDir() {
		if st, err := os.Stat(key); err == nil && !st.IsDir() {
			return cert, key, true
		}
	}
	return "", "", false
}

func agentPairForDevice(dir string, deviceID string) (string, string, string, bool) {
	matches, err := filepath.Glob(filepath.Join(dir, "agent-*-"+deviceID+".crt"))
	if err != nil || len(matches) == 0 {
		return "", "", "", false
	}
	sort.Strings(matches)
	for _, cert := range matches {
		key := strings.TrimSuffix(cert, ".crt") + ".key"
		st, err := os.Stat(key)
		if err != nil || st.IsDir() {
			continue
		}
		base := filepath.Base(strings.TrimSuffix(cert, ".crt"))
		user := strings.TrimSuffix(strings.TrimPrefix(base, "agent-"), "-"+deviceID)
		if user != "" && user != base {
			return cert, key, user, true
		}
	}
	return "", "", "", false
}

func expandUser(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
