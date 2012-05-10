package gitosis

import (
	"fmt"
	"os"
	"path"
	"syscall"
)

// BuildAndStoreKeyFile adds a key to key dir, returning the name
// of the file containing the new public key. This name should
// be stored for future remotion of the key.
func BuildAndStoreKeyFile(member, key string) (string, error) {
	p, err := getKeydirPath()
	if err != nil {
		return "", err
	}
	err = os.MkdirAll(p, 0755)
	if err != nil {
		return "", err
	}
	filename, err := nextAvailableKey(p, member)
	if err != nil {
		return "", err
	}
	keyfilename := path.Join(p, filename)
	keyfile, err := os.OpenFile(keyfilename, syscall.O_WRONLY|syscall.O_CREAT, 0644)
	if err != nil {
		return "", err
	}
	defer keyfile.Close()
	n, err := keyfile.WriteString(key)
	if err != nil || n != len(key) {
		return "", err
	}
	return filename, nil
}

func nextAvailableKey(keydirname, member string) (string, error) {
	keydir, err := os.Open(keydirname)
	if err != nil {
		return "", err
	}
	defer keydir.Close()
	filenames, err := keydir.Readdirnames(0)
	if err != nil {
		return "", err
	}
	pattern := member + "_key%d.pub"
	counter := 1
	filename := fmt.Sprintf(pattern, counter)
	for _, f := range filenames {
		if f == filename {
			counter++
			filename = fmt.Sprintf(pattern, counter)
		}
	}
	return filename, nil
}
