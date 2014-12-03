package main

import (
	"errors"
	"fmt"
	flag "github.com/ogier/pflag"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type RCStatus int
type CHStatus int

const (
	HELP = `Usage: chown [OPTION]... [OWNER][:[GROUP]] FILE...
  or:  chown [OPTION]... --reference=RFILE FILE...
Change the owner and/or group of each FILE to OWNER and/or GROUP.
With --reference, change the owner and group of each FILE to those of RFILE.

  -c, --changes          like verbose but report only when a change is made
  -f, --silent, --quiet  suppress most error messages
  -v, --verbose          output a diagnostic for every file processed
      --dereference      affect the referent of each symbolic link (this is
                         the default), rather than the symbolic link itself
  -h, --no-dereference   affect symbolic links instead of any referenced file
                         (useful only on systems that can change the
                         ownership of a symlink)
      --from=CURRENT_OWNER:CURRENT_GROUP
                         change the owner and/or group of each file only if
                         its current owner and/or group match those specified
                         here.  Either may be omitted, in which case a match
                         is not required for the omitted attribute
      --no-preserve-root  do not treat '/' specially (the default)
      --preserve-root    fail to operate recursively on '/'
      --reference=RFILE  use RFILE's owner and group rather than
                         specifying OWNER:GROUP values
  -R, --recursive        operate on files and directories recursively

The following options modify how a hierarchy is traversed when the -R
option is also specified.  If more than one is specified, only the final
one takes effect.

  -H                     if a command line argument is a symbolic link
                         to a directory, traverse it
  -L                     traverse every symbolic link to a directory
                         encountered
  -P                     do not traverse any symbolic links (default)

      --help     display this help and exit
      --version  output version information and exit

Owner is unchanged if missing.  Group is unchanged if missing, but changed
to login group if implied by a ':' following a symbolic OWNER.
OWNER and GROUP may be numeric as well as symbolic.

Examples:
  chown root /u        Change the owner of /u to "root".
  chown root:staff /u  Likewise, but also change its group to "staff".
  chown -hR root /u    Change the owner of /u and subfiles to "root".

Report wc bugs to ericscottlagergren@gmail.com
Go coreutils home page: <https://www.github.com/EricLagerg/go-coreutils/>`
	VERSION = `chown (Go coreutils) 1.0
Copyright (C) 2014 Eric Lagergren
License GPLv3+: GNU GPL version 3 or later <http://gnu.org/licenses/gpl.html>.
This is free software: you are free to change and redistribute it.
There is NO WARRANTY, to the extent permitted by law.

Written by Eric Lagergren.
Inspired by David MacKenzie and Jim Meyering.`

	ROOT_INODE = 2 // Root inode is 2 on linux (0 is NULL, 1 is bad blocks)
	MAX_INT    = int(^uint(0) >> 1)
)

// Copied from http://golang.org/src/pkg/os/types.go
const (
	// The single letters are the abbreviations
	// used by the String method's formatting.
	ModeDir        uint32 = 1 << (32 - 1 - iota) // d: is a directory
	ModeAppend                                   // a: append-only
	ModeExclusive                                // l: exclusive use
	ModeTemporary                                // T: temporary file (not backed up)
	ModeSymlink                                  // L: symbolic link
	ModeDevice                                   // D: device file
	ModeNamedPipe                                // p: named pipe (FIFO)
	ModeSocket                                   // S: Unix domain socket
	ModeSetuid                                   // u: setuid
	ModeSetgid                                   // g: setgid
	ModeCharDevice                               // c: Unix character device, when ModeDevice is set
	ModeSticky                                   // t: sticky

	// Mask for the type bits. For regular files, none will be set.
	ModeType = ModeDir | ModeSymlink | ModeNamedPipe | ModeSocket | ModeDevice

	ModePerm = 0777 // permission bits
)

const (
	_  = iota // Don't need 0
	__ = iota // Don't need 1

	// fchown succeeded
	RC_OK RCStatus = iota

	// uid/gid are specified and don't match
	RC_EXCLUDED RCStatus = iota

	// SAME_INODE failed
	RC_INODE_CHANGED RCStatus = iota

	// open/fchown isn't needed, safe, or doesn't work so use chown
	RC_DO_ORDINARY_CHOWN RCStatus = iota

	// open, fstat, fchown, or close failed
	RC_ERROR RCStatus = iota
)

const (
	_                               = iota // Don't need 0
	CH_NOT_APPLIED         CHStatus = iota
	CH_SUCCEEDED           CHStatus = iota
	CH_FAILED              CHStatus = iota
	CH_NO_CHANGE_REQUESTED CHStatus = iota
)

var (
	changes   = flag.BoolP("changes", "c", false, "verbose but for changes")
	deref     = flag.Bool("dereference", true, "affect sym link referent")
	noderef   = flag.BoolP("no-dereference", "h", false, "affect sym link rather than linked file")
	from      = flag.String("from", "", "change owner and/or group if owner/group matches. Either may be omitted.")
	npr       = flag.Bool("no-preserve-root", true, "don't treat root '/' specially")
	pr        = flag.Bool("preserve-root", false, "fail recursive operation on '/")
	silent    = flag.BoolP("silent", "f", false, "suppress most error messages")
	silent2   = flag.Bool("quiet", false, "suppress most error messages")
	rfile     = flag.String("reference", "", "use RFILE's owner/group")
	recursive = flag.BoolP("recursive", "R", false, "operate recursively")
	verbose   = flag.BoolP("verbose", "v", false, "diagnostic for each file")
	travDir   = flag.BoolP("N1O1L1O1N1G1O1P1T1", "H", false, "if cli arg is sym link to dir, follow it")
	travAll   = flag.BoolP("N1O1L1O1N1G1O1P1T2", "L", false, "traverse every sym link")
	noTrav    = flag.BoolP("N1O1L1O1N1G1O1P1T3", "P", true, "don't traverse any sym links")
	version   = flag.Bool("version", false, "print program's version\n")
	debug     = flag.BoolP("debug", "d", false, "print cli vars entered")

	optUid = -1 // Specified uid; -1 if not to be changed.
	optGid = -1 // Specified uid; -1 if not to be changed.

	// Change the owner (group) of a file only if it has this uid (gid).
	// -1 means there's no restriction.
	reqUid = -1
	reqGid = -1

	mute          = *silent || *silent2
	DO_NOT_FOLLOW = false

	SkipDir  = errors.New("skip this directory")
	CantFind = errors.New("can't find user/group/uid/gid")
)

func nameToUid(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return -2, CantFind
	}
	i, _ := strconv.Atoi(u.Uid)
	return i, nil
}

func nameToGid(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return -2, CantFind
	}
	i, _ := strconv.Atoi(g.Gid)
	return i, nil
}

func gidToName(gid uint32) (string, error) {
	g, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10))
	if err != nil {
		return "", CantFind
	}
	return g.Name, nil
}

func uidToName(uid uint32) (string, error) {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return "", CantFind
	}
	return u.Username, nil
}

func walk(path string, info os.FileInfo, uid, gid, reqUid, reqGid int) bool {
	var ok bool
	var fileInfo os.FileInfo
	var err error

	sym := false

	ok = ChangeOwner(path, info, uid, gid, reqUid, reqGid)

	names, err := readDirNames(path)
	if err != nil {
		return false
	}

	for _, name := range names {
		filename := filepath.Join(path, name)
		if !*deref {
			fileInfo, err = os.Lstat(filename)
			if fileInfo.Mode()&os.ModeSymlink == os.ModeSymlink {
				sym = true
			}
		} else {
			fileInfo, err = os.Stat(filename)
		}

		if err != nil {
			ok = false
		} else {
			// If we're going to chown the symlink instead of the linked file,
			// we need to do it before we follow the link and continue with
			// our recursive chowning
			if sym && *travAll && fileInfo.IsDir() {
				filename, _ = os.Readlink(filename)
				ok = walk(filename, fileInfo, uid, gid, reqUid, reqGid)
			} else if sym && fileInfo.Mode().IsRegular() {
				ok = ChangeOwner(filename, fileInfo, uid, gid, reqUid, reqGid)
			} else if fileInfo.IsDir() {
				ok = walk(filename, fileInfo, uid, gid, reqUid, reqGid)
			} else {
				ok = ChangeOwner(filename, fileInfo, uid, gid, reqUid, reqGid)
			}
		}
	}
	return ok
}

// from http://golang.org/src/pkg/path/filepath/path.go
func readDirNames(dirname string) ([]string, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// Returns true if chown is successful on all files
func ChownFiles(fname string, uid, gid, reqUid, reqGid int) bool {
	ok := false

	if !*deref {
		fi, err := os.Lstat(fname)
		if err != nil {
			if !mute {
				fmt.Printf("cannot lstat() file or directory '%s'\n", fname)
			}
		}
		if *recursive {
			if walk(fname, fi, uid, gid, reqUid, reqGid) {
				ok = true
			}
		} else {
			if ChangeOwner(fname, fi, uid, gid, reqUid, reqGid) {
				ok = true
			}
		}
	} else {
		fi, err := os.Stat(fname)
		if err != nil {
			if !mute {
				fmt.Printf("cannot stat() file or directory '%s'\n", fname)
			}
		}
		if *recursive {
			if walk(fname, fi, uid, gid, reqUid, reqGid) {
				ok = true
			}
		} else {
			if ChangeOwner(fname, fi, uid, gid, reqUid, reqGid) {
				ok = true
			}
		}
	}

	return ok
}

func ChangeOwner(fname string, origStat os.FileInfo, uid, gid, reqUid, reqGid int) bool {
	var status RCStatus
	var doChown bool
	var changed bool
	var changeStatus CHStatus

	symlinkChanged := true
	ok := true

	fi, err := os.Open(fname)
	if err != nil {
		if !mute {
			fmt.Printf("%v %s", err, fname)
		}
		ok = false
	}

	stat_t := syscall.Stat_t{}
	err = syscall.Stat(fname, &stat_t)

	// TODO: Better error messages, similar to fts(3)'s FTS_DNR, FTS_ERR,
	// and so on
	if err != nil {
		if !mute {
			fmt.Printf("%v %s", err, fname)
		}
		ok = false
	}

	// Check if we've stumbled across a directory while we've
	if stat_t.Mode&ModeDir != 0 && stat_t.Ino == ROOT_INODE {
		if *recursive && *pr {
			fmt.Print("cannot run chown on root directory (--preserve-root specified\n")
			DO_NOT_FOLLOW = true
			return false
		} else {
			fmt.Print("warning: running chown on root directory without protection\n")
		}
	}

	if !ok {
		doChown = false
	} else {
		doChown = true
	}

	if doChown {
		if !*deref {
			err := os.Lchown(fname, uid, gid)

			// GNU chown.c returns true if the operation isn't supported.
			// It's a POSIX thing.
			if err != nil {
				ok = true
				symlinkChanged = false
			}
		} else {
			// Double check the size of the fd because the Go's openat() wants
			// the cwd_fd to be of the int type, while os.File's Fd() returns
			// a uintptr() of the fd. Theoretically, this means that the value
			// returned from Fd() could be out of the bounds of our int, thus
			// causing us to accidentally chown incorrectly.
			//
			// os.File.Fd() is the only way that I know how to get the fd without
			// actually using openat() -- which we can't do without a fd
			//
			// Apparently there's a soft restriction of ~4 billion inodes
			// which is set when the filesystem is created
			// Since int in Go is >= 32 bits, we have a range of
			// -2147483648 through 2147483647, which means we have about 1.8
			// billion inodes that we cannot (assuming 32 bit int size) run
			// RestrictedChown() on. That said, I'd be willing to bet most
			// systems do not have > 2147483647 inodes. If a system does,
			// we'll have to hope that said system is using 64 bit ints.
			// (Which is becoming more and more common.)
			if fi.Fd() <= uintptr(MAX_INT) {
				status = RestrictedChown(int(fi.Fd()), fname, origStat, uid, gid, reqUid, reqGid)
			} else {
				panic("Go sucks, use C (just kidding)")
			}

			switch status {
			case RC_OK:
				break
			case RC_DO_ORDINARY_CHOWN:
				if !*deref {
					err := os.Lchown(fname, uid, gid)
					if err != nil {
						ok = false
						if os.IsPermission(err) {
							if !mute {
								fmt.Printf("chown: changing ownership of '%s' not permitted\n", fname)
							}
						}
					} else {
						ok = true
					}
				} else {
					err := os.Chown(fname, uid, gid)
					if err != nil {
						ok = false
						if os.IsPermission(err) {
							if !mute {
								fmt.Printf("chown: changing ownership of '%s' not permitted\n", fname)
							}
						}
					} else {
						ok = true
					}
				}
			case RC_ERROR:
				ok = false
			case RC_INODE_CHANGED:
				fmt.Printf("inode changed during chown of '%s'\n", fname)
				fallthrough
			case RC_EXCLUDED:
				doChown = false
				ok = false
			default:
				panic("Now how did this happen?")
			}
		}
	}

	if !mute && *verbose || *changes {
		if changed = doChown && ok && (symlinkChanged && uid != -1 || uint32(uid) == stat_t.Uid) && (gid != -1 || uint32(gid) == stat_t.Gid); changed || *verbose {

			if !ok {
				changeStatus = CH_FAILED
			} else if !symlinkChanged {
				changeStatus = CH_NOT_APPLIED
			} else if !changed {
				changeStatus = CH_NO_CHANGE_REQUESTED
			} else {
				changeStatus = CH_SUCCEEDED
			}

			oldUsr, _ := uidToName(stat_t.Uid)
			oldGroup, _ := gidToName(stat_t.Gid)
			newUsr, _ := uidToName(uint32(optUid))
			newGrp, _ := gidToName(uint32(optGid))

			DescribeChange(fname, changeStatus, oldUsr, oldGroup, newUsr, newGrp)
		}
	}

	return ok
}

func RestrictedChown(cwd_fd int, file string, origStat os.FileInfo, uid, gid, reqUid, reqGid int) RCStatus {
	var status RCStatus

	openFlags := syscall.O_NONBLOCK | syscall.O_NOCTTY

	fstat := syscall.Stat_t{}
	err := syscall.Stat(file, &fstat)

	fileInfo, err := os.Stat(file)
	fileMode := fileInfo.Mode()

	if reqUid == -1 && reqGid == -1 {
		return RC_DO_ORDINARY_CHOWN
	}

	if !fileMode.IsRegular() {
		if fileMode.IsDir() {
			openFlags |= syscall.O_DIRECTORY
		} else {
			return RC_DO_ORDINARY_CHOWN
		}
	}

	fd, errno := syscall.Openat(cwd_fd, file, syscall.O_RDONLY|openFlags, 0)

	if !(0 <= fd || errno != nil && fileMode.IsRegular()) {
		if fd, err = syscall.Openat(cwd_fd, file, syscall.O_WRONLY|openFlags, 0); !(0 <= fd) {
			if err != nil {
				return RC_DO_ORDINARY_CHOWN
			} else {
				return RC_ERROR
			}
		}
	}

	if err := syscall.Fstat(fd, &fstat); err != nil {
		status = RC_ERROR
	} else if !os.SameFile(origStat, fileInfo) {
		status = RC_INODE_CHANGED
	} else if reqUid == -1 || uint32(reqUid) == fstat.Uid && reqGid == -1 || uint32(reqGid) == fstat.Gid { // Sneaky chown lol
		if err := syscall.Fchown(fd, uid, gid); err == nil {
			if err := syscall.Close(fd); err == nil {
				return RC_OK
			} else {
				return RC_ERROR
			}
		} else {
			if os.IsPermission(err) {
				fmt.Printf("chown: changing ownership of '%s' not permitted\n", fd)
			}
			status = RC_ERROR
		}
	}
	err = syscall.Close(fd)
	if err != nil {
		panic(err)
	}
	return status
}

func DescribeChange(file string, changed CHStatus, olduser, oldgroup, user, group string) {

	userbool := false
	groupbool := false

	if user != "" {
		userbool = true
	}
	if group != "" {
		groupbool = true
	}

	if changed == CH_NOT_APPLIED {
		fmt.Printf("neither symbolic link '%s' nor referent has been changed\n", file)
	}

	spec := fmt.Sprintf("'%s:%s'", user, group)
	var oldspec string
	if userbool {
		if groupbool {
			oldspec = fmt.Sprintf("'%s:%s'", olduser, oldgroup)
		} else {
			oldspec = fmt.Sprintf("'%s:%s'", olduser, nil)
		}
	} else if groupbool {
		oldspec = fmt.Sprintf("'%s:%s'", nil, oldgroup)
	} else {
		oldspec = ""
	}

	switch changed {
	case CH_SUCCEEDED:
		if userbool {
			fmt.Printf("changed ownership of '%s' from %s to %s\n", file, oldspec, spec)
		} else if groupbool {
			fmt.Printf("changed group of '%s' from %s to %s\n", file, oldspec, spec)
		} else {
			fmt.Printf("no change in ownership of %s\n", file)
		}
	case CH_FAILED:
		if oldspec != "" {
			if userbool {
				fmt.Printf("failed to change ownership of '%s' from %s to %s\n", file, oldspec, spec)
			} else if groupbool {
				fmt.Printf("failed to change group of '%s' from %s to %s\n", file, oldspec, spec)
			} else {
				fmt.Printf("failed to change ownership of %s", file)
			}
		} else {
			if userbool {
				fmt.Printf("failed to change ownership of '%s' from %s to %s\n", file, oldspec, spec)
			} else if groupbool {
				fmt.Printf("failed to change group of '%s' from %s to %s\n", file, oldspec, spec)
			} else {
				fmt.Printf("failed to change ownership of %s", file)
			}
			oldspec = spec
		}
	case CH_NO_CHANGE_REQUESTED:
		if userbool {
			fmt.Printf("ownership of '%s' retained as %s\n", file, oldspec)
		} else if groupbool {
			fmt.Printf("ownership of '%s' retained as %s\n", file, oldspec)
		} else {
			fmt.Printf("ownership of '%s' retained\n", file)
		}
	default:
		panic("let's go out with a bang!") // TODO: Good error messages lol
	}
}

// We have to do extra arg parsing here because chown doesn't use the
// standard CLI format that other utilities do
// For instance...
// chown -R eric:root /home/eric/documents
//
// compared to...
//
// grep -r 'regex'
//
// Our flags parser (pflag) *can* handle this format, but need to split the
// string(s) (e.g. eric:root -> args[0] == eric && args[1] == root)
func main() {
	var a []string
	var b []string
	var inFile string
	var shopts bool // Which file is the starting file?
	var u string
	var g string

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s\n", HELP)
		os.Exit(0)
	}
	flag.Parse()
	if *version {
		fmt.Fprintf(os.Stderr, "%s\n", VERSION)
		os.Exit(0)
	}
	args := flag.Args()

	if *rfile != "" {
		shopts = true
	}

	if len(args) < 2 && !shopts {
		fmt.Fprint(os.Stderr, "You forgot an argument! (Or two. Or more.)\nIn case you forgot how to use the program, here's how:\n\n")
		fmt.Fprintf(os.Stderr, "%s\n", HELP)
		os.Exit(1)
	}

	if shopts {
		inFile = args[0]
	} else {
		inFile = args[1]
	}

	if *rfile != "" && *from == "" {
		stat_t := syscall.Stat_t{}
		err := syscall.Stat(*rfile, &stat_t)
		if err != nil {
			fmt.Printf("cannot stat rfile '%s'", *rfile)
		}
		u = strconv.FormatUint(uint64(stat_t.Uid), 10)
		g = strconv.FormatUint(uint64(stat_t.Gid), 10)
	} else {
		a = strings.Split(args[0], ":")
	}

	if *from != "" {
		b = strings.Split(*from, ":")
		// If input is a uid
		if ok, err := strconv.Atoi(b[1]); err == nil {
			if _, err = uidToName(uint32(ok)); err == nil {
				reqUid = ok
			}
			// If it's a username
		} else if ok, err := nameToUid(b[1]); err == nil {
			reqUid = ok
			// If nothing supplied
		} else if b[1] == "" {
			reqUid = -1
			// Woohoo errors!!1!
		} else {
			fmt.Fprintf(os.Stderr, "invalid username/uid %s\n", b[1])
			os.Exit(1)
		}

		// If input is a gid
		if ok, err := strconv.Atoi(b[1]); err == nil {
			if _, err = gidToName(uint32(ok)); err == nil {
				reqGid = ok
			}
			// If it's a groupname
		} else if ok, err := nameToGid(b[1]); err == nil {
			reqGid = ok
			// If nothing supplied
		} else if b[1] == "" {
			reqGid = -1
			// Woohoo errors!!1!
		} else {
			fmt.Fprintf(os.Stderr, "invalid groupame/gid %s\n", b[1])
			os.Exit(1)
		}
	}

	if *rfile == "" {
		u = a[0]
		g = a[1]
	}

	// If input is a uid
	if ok, err := strconv.Atoi(u); err == nil {
		if _, err = uidToName(uint32(ok)); err == nil {
			optUid = ok
		}
		// If it's a username
	} else if ok, err := nameToUid(u); err == nil {
		optUid = ok
		// If nothing supplied
	} else if u == "" {
		optUid = -1
		// Woohoo errors!!1!
	} else {
		fmt.Fprintf(os.Stderr, "invalid username/uid %s\n", u)
		os.Exit(1)
	}

	// If input is a gid
	if ok, err := strconv.Atoi(g); err == nil {
		if _, err = gidToName(uint32(ok)); err == nil {
			optGid = ok
		}
		// If it's a groupname
	} else if ok, err := nameToGid(g); err == nil {
		optGid = ok
		// If nothing supplied
	} else if g == "" {
		optGid = -1
		// Woohoo errors!!1!
	} else {
		fmt.Fprintf(os.Stderr, "invalid groupame/gid %s\n", g)
		os.Exit(1)
	}

	if *noderef {
		*deref = false
	}

	if *recursive && *deref && !*travDir && !*travAll {
		fmt.Fprint(os.Stderr, "-R --dereference requires either -H or -L\n")
		os.Exit(1)
	}

	if *debug { // Mainly for me
		fmt.Printf("%v %v %v %v %v\n", inFile, optUid, optGid, reqUid, reqGid)
	}

	if !ChownFiles(inFile, optUid, optGid, reqUid, reqGid) {
		os.Exit(1) // Exit 1 if any files aren't chowned
	}
}
