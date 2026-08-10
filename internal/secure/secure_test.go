package secure

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("a-very-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "a-very-long-password") {
		t.Fatal("password did not verify")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Fatal("wrong password verified")
	}
}

func TestVaultRoundTrip(t *testing.T) {
	vault, err := NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := vault.Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vault.Decrypt(encoded)
	if err != nil || plain != "secret" {
		t.Fatalf("got %q, %v", plain, err)
	}
}
