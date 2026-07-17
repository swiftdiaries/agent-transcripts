//go:build darwin

package discovery

import "golang.org/x/sys/unix"

func fileIdentityFromStat(stat *unix.Stat_t) fileIdentity {
	return fileIdentity{Device: uint64(stat.Dev), Inode: stat.Ino, Size: stat.Size, ModTimeNS: stat.Mtim.Sec*1_000_000_000 + stat.Mtim.Nsec, ChangeTimeNS: stat.Ctim.Sec*1_000_000_000 + stat.Ctim.Nsec}
}
