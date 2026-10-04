package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// loadSMBUsersFile adds the accounts in smb.users_file to those in the
// configuration itself. It returns a notice to log if the file can be read
// by users other than its owner.
func loadSMBUsersFile(smb *SMBConfig, configFile string) (string, error) {
	path := smb.UsersFile
	if !filepath.IsAbs(path) && configFile != "" {
		path = filepath.Join(filepath.Dir(configFile), path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("smb.users_file: %w", err)
	}

	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil {
		return "", fmt.Errorf("smb.users_file %s: %w", path, err)
	}
	var file struct {
		Users []SMBUser `mapstructure:"users"`
	}
	if err := v.UnmarshalExact(&file); err != nil {
		return "", fmt.Errorf("smb.users_file %s: %w", path, err)
	}
	if len(file.Users) == 0 {
		return "", fmt.Errorf("smb.users_file %s: no accounts under \"users\"", path)
	}
	smb.Users = append(smb.Users, file.Users...)

	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("smb.users_file %s is readable by users other than its owner (mode %04o); it holds account hashes, so restrict it with chmod 600.",
			path, info.Mode().Perm()), nil
	}
	return "", nil
}
