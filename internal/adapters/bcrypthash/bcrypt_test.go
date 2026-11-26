package bcrypthash_test

import (
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/bshadmehr98/candledb/internal/adapters/bcrypthash"
)

func TestHashAndVerify(t *testing.T) {
	h := bcrypthash.New(bcrypt.MinCost)
	hash, err := h.Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse" {
		t.Fatal("the hash is the password")
	}
	if !h.Verify(hash, "correct horse") {
		t.Error("the right password was rejected")
	}
	if h.Verify(hash, "wrong") || h.Verify("not a hash", "correct horse") {
		t.Error("a wrong password or a malformed hash was accepted")
	}
	// A random salt makes every hash different, so equal passwords cannot
	// be spotted by comparing hashes.
	again, _ := h.Hash("correct horse")
	if again == hash {
		t.Error("hashing the same password twice gave the same hash")
	}
}
