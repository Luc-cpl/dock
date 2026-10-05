package dock

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestBrowserTrustDatabases(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "custom-config")
	data := filepath.Join(home, "custom-data")
	root := filepath.Join(config, "mozilla/firefox")
	external := t.TempDir()
	paths := []string{filepath.Join(data, "pki/nssdb"), filepath.Join(root, "default-release"), external,
		filepath.Join(home, ".var/app/com.google.Chrome/data/pki/nssdb")}
	var want []string
	for _, path := range paths {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cert9.db"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		want = append(want, "sql:"+path)
	}
	if err := os.Symlink(paths[1], filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	ini := "[Profile0]\nPath=default-release\nIsRelative=1\n[Profile1]\nIsRelative=0\nPath=" + external + "\n"
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(ini), 0600); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	if got := browserTrustDatabases(home, config, data); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestInstallNSSCertificate(t *testing.T) {
	if _, err := exec.LookPath("certutil"); err != nil {
		t.Skip("certutil is unavailable")
	}
	dir := t.TempDir()
	db := "sql:" + dir
	if output, err := exec.Command("certutil", "-N", "-d", db, "--empty-password").CombinedOutput(); err != nil {
		t.Fatalf("initialize NSS: %v: %s", err, output)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(12345), Subject: pkix.Name{CommonName: "Test development CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "root.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := installNSSCertificate(db, caPath, "test CA", der); err != nil {
			t.Fatal(err)
		}
	}
	if err := installNSSCertificate(db, caPath, "test CA", []byte("different CA")); err == nil {
		t.Fatal("expected CA mismatch error")
	}
	if err := installNSSCertificate("sql:"+filepath.Join(dir, "missing"), caPath, "test CA", der); err == nil {
		t.Fatal("expected missing database error")
	}
}
