package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
)

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	seed, err := os.ReadFile(path)
	if err == nil && len(seed) == ed25519.SeedSize {
		stripDarwinACL(path)
		_ = os.Chmod(path, 0o600)
		priv := ed25519.NewKeyFromSeed(seed)
		if err := writeDaemonPub(path, priv); err != nil {
			return nil, err
		}
		return priv, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
		return nil, err
	}
	stripDarwinACL(path)
	_ = os.Chmod(path, 0o600)
	if err := writeDaemonPub(path, priv); err != nil {
		return nil, err
	}
	return priv, nil
}

func writeDaemonPub(keyPath string, priv ed25519.PrivateKey) error {
	pubKey, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return os.ErrInvalid
	}
	dir := filepath.Dir(keyPath)
	pubPath := filepath.Join(dir, "daemon.pub")
	if err := os.WriteFile(pubPath, pubKey, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(pubPath, 0o600); err != nil {
		return err
	}
	if isSharedPrefixDir(dir) {
		grantDarwinUserReadACL(pubPath)
	}
	return nil
}

func isSharedPrefixDir(dir string) bool {
	clean := filepath.Clean(dir)
	return filepath.Base(clean) == "agent-machine" && filepath.Base(filepath.Dir(clean)) == "var"
}

func stripDarwinACL(path string) {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("/bin/chmod", "-N", path).Run()
}

func grantDarwinUserReadACL(path string) {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("/bin/chmod", "-N", path).Run()
	for _, name := range []string{"admin", "user"} {
		if agentAccount(name) {
			continue
		}
		if _, err := user.Lookup(name); err == nil {
			_ = exec.Command("/bin/chmod", "+a", "user:"+name+" allow read,readattr,readextattr,readsecurity", path).Run()
		}
	}
}

func grantDarwinUserSocketACL(sock string) {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("/bin/chmod", "-N", sock).Run()
	for _, name := range []string{"admin", "user"} {
		if agentAccount(name) {
			continue
		}
		if _, err := user.Lookup(name); err == nil {
			_ = exec.Command("/bin/chmod", "+a", "user:"+name+" allow read,write,readattr,readextattr,readsecurity", sock).Run()
		}
	}
}

func applyDarwinSharedDirACL(dir, peers string) {
	if runtime.GOOS != "darwin" || !isSharedPrefixDir(dir) {
		return
	}
	_ = exec.Command("/bin/chmod", "-N", dir).Run()
	if _, err := user.Lookup("admin"); err == nil {
		_ = exec.Command("/bin/chmod", "+a", "user:admin allow list,add_file,search,delete,add_subdirectory,delete_child,readattr,writeattr,readextattr,writeextattr,readsecurity,read,write,append,file_inherit", dir).Run()
	}
	if _, err := user.Lookup("user"); err == nil {
		_ = exec.Command("/bin/chmod", "+a", "user:user allow list,add_file,search,readattr,readextattr,readsecurity", dir).Run()
		_ = exec.Command("/bin/chmod", "+a", "user:user allow read,write,delete,append,readattr,writeattr,readextattr,writeextattr,readsecurity,file_inherit,only_inherit", dir).Run()
	}
	if peers != "" {
		_ = exec.Command("/bin/chmod", "-N", peers).Run()
		for _, name := range []string{"admin", "user"} {
			if agentAccount(name) {
				continue
			}
			if _, err := user.Lookup(name); err == nil {
				_ = exec.Command("/bin/chmod", "+a", "user:"+name+" allow list,add_file,search,delete,add_subdirectory,delete_child,readattr,writeattr,readextattr,writeextattr,readsecurity,read,write,append,file_inherit,directory_inherit", peers).Run()
			}
		}
	}
}

func loadPublic(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, os.ErrInvalid
	}
	return ed25519.PublicKey(b), nil
}
