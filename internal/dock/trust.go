package dock

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func Trust() error {
	if _, err := exec.LookPath("mkcert"); err != nil {
		return errors.New("mkcert is not installed; install mkcert and try again")
	}
	output, err := exec.Command("mkcert", "-install").CombinedOutput()
	// Some mkcert versions miss newer browser paths. Only tolerate that specific
	// failure when mkcert also confirms that system trust succeeded.
	nssUnavailable := strings.Contains(string(output), "no Firefox and/or Chrome/Chromium security databases found")
	systemTrusted := strings.Contains(string(output), "installed in the system trust store")
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.Contains(line, "no Firefox and/or Chrome/Chromium security databases found") && systemTrusted {
			fmt.Fprintln(os.Stdout, "Note: mkcert did not find legacy browser databases; Dock will check current browser paths.")
			continue
		}
		fmt.Fprintln(os.Stdout, line)
	}
	if err != nil && !(nssUnavailable && systemTrusted) {
		return fmt.Errorf("mkcert -install: %w", err)
	}
	caroot, err := exec.Command("mkcert", "-CAROOT").Output()
	if err != nil {
		return fmt.Errorf("get mkcert CA path: %w", err)
	}
	if err := installBrowserTrust(filepath.Join(strings.TrimSpace(string(caroot)), "rootCA.pem")); err != nil {
		return err
	}
	return generateLocalCertificate(DataDir(), "localhost", "127.0.0.1", "::1")
}

// Discover only existing browser databases; never create profiles or empty stores.
func browserTrustDatabases(home, configDir, dataDir string) []string {
	paths := []string{filepath.Join(home, ".pki/nssdb"), filepath.Join(dataDir, "pki/nssdb"),
		filepath.Join(home, "snap/chromium/current/.pki/nssdb")}
	profileRoots := []string{filepath.Join(home, ".mozilla/firefox"), filepath.Join(configDir, "mozilla/firefox"),
		filepath.Join(home, "snap/firefox/common/.mozilla/firefox"),
		filepath.Join(home, ".var/app/org.mozilla.firefox/.mozilla/firefox"),
		filepath.Join(home, ".var/app/org.mozilla.firefox/config/mozilla/firefox")}
	for _, app := range []string{"com.google.Chrome", "com.microsoft.Edge", "org.chromium.Chromium"} {
		paths = append(paths, filepath.Join(home, ".var/app", app, ".pki/nssdb"),
			filepath.Join(home, ".var/app", app, "data/pki/nssdb"))
	}
	for _, root := range profileRoots {
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			paths = append(paths, filepath.Join(root, entry.Name()))
		}
		// profiles.ini can reference profiles outside the default directory.
		ini, _ := os.ReadFile(filepath.Join(root, "profiles.ini"))
		for _, section := range strings.Split(string(ini), "[") {
			if !strings.HasPrefix(section, "Profile") {
				continue
			}
			var path string
			relative := true
			for _, line := range strings.Split(section, "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
				if !ok {
					continue
				}
				switch strings.TrimSpace(key) {
				case "Path":
					path = strings.TrimSpace(value)
				case "IsRelative":
					relative = strings.TrimSpace(value) != "0"
				}
			}
			if path != "" {
				if relative {
					path = filepath.Join(root, path)
				}
				paths = append(paths, path)
			}
		}
	}
	seen := map[string]bool{}
	var databases []string
	for _, path := range paths {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		for _, format := range []struct{ file, prefix string }{{"cert9.db", "sql:"}, {"cert8.db", "dbm:"}} {
			if stat, err := os.Stat(filepath.Join(path, format.file)); err == nil && stat.Mode().IsRegular() {
				db := format.prefix + path
				if !seen[db] {
					databases = append(databases, db)
					seen[db] = true
				}
				break
			}
		}
	}
	sort.Strings(databases)
	return databases
}

func installBrowserTrust(caPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local/share")
	}
	databases := browserTrustDatabases(home, configDir, dataDir)
	if len(databases) == 0 {
		fmt.Fprintln(os.Stdout, "No browser certificate databases found. Start your browsers once, then run dock trust again.")
		return nil
	}
	if _, err := exec.LookPath("certutil"); err != nil {
		return errors.New("browser trust requires certutil; on Debian/Ubuntu install libnss3-tools, then run dock trust again")
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return errors.New("invalid mkcert root CA PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	// Match mkcert's nickname to keep repeated installs idempotent.
	nickname := "mkcert development CA " + ca.SerialNumber.String()
	var failures []error
	for _, db := range databases {
		if err := installNSSCertificate(db, caPath, nickname, ca.Raw); err != nil {
			failures = append(failures, err)
			continue
		}
		fmt.Fprintf(os.Stdout, "Browser CA trust verified: %s\n", db)
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "Restart browsers completely to load the updated certificate trust.")
	return nil
}

func installNSSCertificate(db, caPath, nickname string, expectedDER []byte) error {
	if output, err := exec.Command("certutil", "-A", "-d", db, "-t", "C,,", "-n", nickname, "-i", caPath).CombinedOutput(); err != nil {
		return fmt.Errorf("install browser CA in %s: %w: %s", db, err, strings.TrimSpace(string(output)))
	}
	output, err := exec.Command("certutil", "-L", "-d", db, "-n", nickname, "-a").CombinedOutput()
	if err != nil {
		return fmt.Errorf("read browser CA in %s: %w: %s", db, err, strings.TrimSpace(string(output)))
	}
	block, _ := pem.Decode(output)
	if block == nil || !bytes.Equal(block.Bytes, expectedDER) {
		return fmt.Errorf("browser CA mismatch in %s", db)
	}
	if output, err := exec.Command("certutil", "-V", "-d", db, "-u", "L", "-n", nickname).CombinedOutput(); err != nil {
		return fmt.Errorf("verify browser CA trust in %s: %w: %s", db, err, strings.TrimSpace(string(output)))
	}
	return nil
}
