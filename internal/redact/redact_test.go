package redact

import (
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

	// Seven values are long enough to be secrets; "v1alpha1" is not one.
	if got := r.Count(); got != 7 {
		t.Errorf("Count() = %d, want 7", got)
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
