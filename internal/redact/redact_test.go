package redact

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A Talos secrets bundle, shaped like the real thing: a couple of join tokens,
// the cluster's identity, and PEM in base64.
const bundle = `cluster:
    id: PSY1bKb2Sq5tiN2JAnuKRBJ1zP5aaaaaaaaaaaaaaaa=
    secret: rRzNVtMkva9wVop6Br4VVMDUGmKEbbbbbbbbbbbbbbb=
secrets:
    bootstraptoken: 230zuo.e4niuzckys47lfik
    secretboxencryptionsecret: u5R4Qjpq1KVfIBIByGrmxsSga6lccccccccccccccc=
trustdinfo:
    token: rsy6n9.ciqxvgju37njryqs
certs:
    os:
        crt: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJQakNCOGFBREFnRUM=
        key: LS0tLS1CRUdJTiBFRDI1NTE5IFBSSVZBVEUgS0VZLS0tLS0K
version: v1alpha1
`

func TestFromBundleHidesEveryValueItKnows(t *testing.T) {
	r, err := FromBundle([]byte(bundle))
	if err != nil {
		t.Fatal(err)
	}

	// Seven values are long enough to be secrets; "v1alpha1" is not one. The
	// eighth is the body of the certificate, as it reads once decoded.
	if got := r.Count(); got != 8 {
		t.Errorf("Count() = %d, want 8", got)
	}

	// What a dry run actually prints, in the shape talosctl prints it.
	diff := `Dry run summary:
Config diff:
+    token: 230zuo.e4niuzckys47lfik
+    ca:
+        crt: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJQakNCOGFBREFnRUM=
+        key: ""
+    token: rsy6n9.ciqxvgju37njryqs
+  id: PSY1bKb2Sq5tiN2JAnuKRBJ1zP5aaaaaaaaaaaaaaaa=
     hostname: talos-w01
`

	got := r.String(diff)

	for _, secret := range []string{
		"230zuo.e4niuzckys47lfik",
		"rsy6n9.ciqxvgju37njryqs",
		"LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJQakNCOGFBREFnRUM=",
		"PSY1bKb2Sq5tiN2JAnuKRBJ1zP5aaaaaaaaaaaaaaaa=",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("a secret survived redaction:\n%s", got)
		}
	}

	// The diff still reads as a diff: structure, field names and anything
	// that was never secret are untouched.
	for _, keep := range []string{"Config diff:", "token:", "hostname: talos-w01", Marker} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed %q, which is not a secret:\n%s", keep, got)
		}
	}
}

// Redaction is by value, so it is exact: it does not care which field a secret
// appears under, and it does not touch text that merely looks sensitive.
func TestRedactionIsByValue(t *testing.T) {
	r, err := FromBundle([]byte(bundle))
	if err != nil {
		t.Fatal(err)
	}

	elsewhere := "somewhere else entirely: 230zuo.e4niuzckys47lfik"
	if strings.Contains(r.String(elsewhere), "230zuo") {
		t.Error("a secret under an unexpected field was not found")
	}

	innocent := "token: not-a-real-token\nca: none\nversion: v1alpha1"
	if got := r.String(innocent); got != innocent {
		t.Errorf("redacted something it does not know:\n%s", got)
	}
}

func TestFromBundleRefusesWhatItCannotRead(t *testing.T) {
	if _, err := FromBundle([]byte("\t: not yaml at all")); err == nil {
		t.Error("expected an error for a bundle that does not parse")
	}

	// Nothing long enough to be a secret is not a bundle talman can hide
	// anything with, and silently hiding nothing would be the worst outcome.
	if _, err := FromBundle([]byte("version: v1alpha1\n")); err == nil {
		t.Error("expected an error for a bundle with no secrets in it")
	}
}

// A nil redactor is what --redact-secrets=false leaves behind, and it must
// pass text through rather than panic.
func TestNilRedactorIsTransparent(t *testing.T) {
	var r *Redactor

	if got := r.String("token: 230zuo.e4niuzckys47lfik"); got != "token: 230zuo.e4niuzckys47lfik" {
		t.Errorf("nil redactor changed the text: %q", got)
	}

	if r.Count() != 0 {
		t.Error("nil redactor claims to know secrets")
	}
}

// Talos writes some of the bundle's keys into a machine config decoded: the
// Kubernetes CA and service-account keys are PEM blocks of their own. A diff
// prints each line of such a block with its own prefix and indentation, so
// every line of the body has to be found on its own.
func TestDecodedKeysAreHiddenLineByLine(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n" +
		"VTLw7onLRnrq0/IzW7yWR7QkrmBL7jTKEn5u+qKhbwKfBstIs+bMY2Zkp18gnTxK\n" +
		"LxoS2tFczGkPLPgizskuemMghRniWaoLcyehkd3qqGElvW/VDL5AaWTg0nLVkjRo\n" +
		"9z+4Ag==\n" +
		"-----END RSA PRIVATE KEY-----\n"

	r, err := FromBundle([]byte("certs:\n  k8sserviceaccount:\n    key: " +
		base64.StdEncoding.EncodeToString([]byte(pem)) + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	var diff strings.Builder

	diff.WriteString("+    privateKey: |\n")

	for _, line := range strings.Split(strings.TrimSpace(pem), "\n") {
		diff.WriteString("+        " + line + "\n")
	}

	got := r.String(diff.String())

	for _, line := range strings.Split(pem, "\n")[1:5] {
		if strings.Contains(got, line) {
			t.Errorf("a line of the key survived: %q\n%s", line, got)
		}
	}

	// The armour is the same in every block and hides nothing.
	if !strings.Contains(got, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Errorf("the PEM armour was redacted:\n%s", got)
	}
}

// A patch the operator encrypted with SOPS says which of its values are
// secrets: the ones SOPS encrypted, however short, and none of the others.
func TestEncryptedValuesAreTheOnesSOPSEncrypted(t *testing.T) {
	ciphertext := `machine:
    registries:
        config:
            registry.example.internal:
                auth:
                    username: ENC[AES256_GCM,data:abc,iv:x,tag:y,type:str]
                    password: ENC[AES256_GCM,data:def,iv:x,tag:y,type:str]
                    endpoint: registry.example.internal
sops:
    version: 3.13.3
    lastmodified: "2026-09-20T10:00:00Z"
`
	plaintext := `machine:
    registries:
        config:
            registry.example.internal:
                auth:
                    username: ci-robot
                    password: hunter2x
                    endpoint: registry.example.internal
`

	s := NewSet()

	if err := s.AddEncrypted([]byte(ciphertext), []byte(plaintext)); err != nil {
		t.Fatal(err)
	}

	got := s.Redactor().String(plaintext)

	for _, secret := range []string{"ci-robot", "hunter2x"} {
		if strings.Contains(got, secret) {
			t.Errorf("an encrypted value survived: %q\n%s", secret, got)
		}
	}

	if !strings.Contains(got, "endpoint: registry.example.internal") {
		t.Errorf("a value SOPS left in the clear was redacted:\n%s", got)
	}
}

// Values a template read from the environment are added as they come.
func TestAddedValuesAreHidden(t *testing.T) {
	s := NewSet()
	s.Add("tskey-auth-kQx7aBcDeFgHiJ", "short")

	got := s.Redactor().String("authKey: tskey-auth-kQx7aBcDeFgHiJ\nmode: short")

	if strings.Contains(got, "tskey") {
		t.Errorf("an added value survived:\n%s", got)
	}

	if !strings.Contains(got, "mode: short") {
		t.Errorf("a short word was treated as a secret:\n%s", got)
	}
}
