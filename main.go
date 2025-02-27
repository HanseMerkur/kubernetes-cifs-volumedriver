package main

import (
	"github.com/pkg/errors"

	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const (
	retStatSuccess             = "Success"
	retStatFailure             = "Failure"
	retStatNotSupported        = "Not supported"
	retMsgInsufficientArgs     = "Insufficient arguments"
	retMsgUnsupportedOperation = "Unsupported operation"
	retMsgInvalidMounterArgs   = "Invalid mounter arguments"
)

const logFileName = "/var/log/kubernetes-cifs-volumedriver.log"

// returnMsg is the response given back to k8s
type returnMsg struct {
	Status       string
	Message      string
	Capabilities capabilities
}

// Part of the repsonse that informs the driver's capabilities
type capabilities struct {
	Attach          bool
	FSGroup         bool
	SupportsMetrics bool

	// TODO: Check if these capabilities make sense for this driver.
	// SELinuxRelabel   bool
	// RequiresFSResize bool
}

// arguments passed by k8 to this driver
type mounterArgs struct {
	FsGroup          string `json:"kubernetes.io/mounterArgs.FsGroup"`
	FsGroupLegacy    string `json:"kubernetes.io/fsGroup"` // k8s prior to 1.15
	FsType           string `json:"kubernetes.io/fsType"`
	PodName          string `json:"kubernetes.io/pod.name"`
	PodNamespace     string `json:"kubernetes.io/pod.namespace"`
	PodUID           string `json:"kubernetes.io/pod.uid"`
	PvName           string `json:"kubernetes.io/pvOrVolumeName"`
	ReadWrite        string `json:"kubernetes.io/readwrite"`
	ServiceAccount   string `json:"kubernetes.io/serviceAccount.name"`
	MountOptions     string `json:"mountOptions"`
	Opts             string `json:"opts"`
	Server           string `json:"server"`
	Share            string `json:"share"`
	Source           string `json:"source"`
	PasswdMethod     string `json:"passwdMethod"`
	CredentialDomain string `json:"kubernetes.io/secret/domain"`
	CredentialUser   string `json:"kubernetes.io/secret/username"`
	CredentialPass   string `json:"kubernetes.io/secret/password"`
}

func argsContain(args []string, item string) bool {
	for _, arg := range args {
		if arg == item {
			return true
		}
	}
	return false
}

func unmarshalMounterArgs(s string) (ma mounterArgs) {
	ma = mounterArgs{}
	err := json.Unmarshal([]byte(s), &ma)
	if err != nil {
		panic(fmt.Sprintf("Error interpreting mounter args: %s", err))
	}
	if ma.CredentialDomain != "" {
		decoded, err := base64.StdEncoding.DecodeString(ma.CredentialDomain)
		if err != nil {
			panic(fmt.Sprintf("Error decoding credential domain: %s", err))
		}
		ma.CredentialDomain = string(decoded)
	}
	if ma.CredentialUser != "" {
		decoded, err := base64.StdEncoding.DecodeString(ma.CredentialUser)
		if err != nil {
			panic(fmt.Sprintf("Error decoding credential user: %s", err))
		}
		ma.CredentialUser = string(decoded)
	}
	if ma.CredentialPass != "" {
		decoded, err := base64.StdEncoding.DecodeString(ma.CredentialPass)
		if err != nil {
			panic(fmt.Sprintf("Error decoding credential password: %s", err))
		}
		ma.CredentialPass = string(decoded)
	}

	// If we got fsGroup from the legacy json field, assume k8s prior to 1.15
	if ma.FsGroupLegacy != "" {
		ma.FsGroup = ma.FsGroupLegacy
	}
	return
}

func runCommand(cmd *exec.Cmd) error {
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b

	if err := cmd.Start(); err != nil {
		return errors.Wrapf(err, "Error start cmd [cmd=%s]", cmd)
	}

	if err := cmd.Wait(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			status, ok := exiterr.Sys().(syscall.WaitStatus)
			if ok && status.ExitStatus() == 13 {
				// Failed to authenticate against CIFS Server
				return errors.Wrapf(err, "Permission denied for cmd [cmd=%s] [response=%s]", cmd, b.String())
			} else if ok && status.ExitStatus() == 5 && argsContain(cmd.Args, "nodfs") {
				// Input/Output Error with Code 5 plus a nodfs option is almost certainly a DFS-Share failure
				return errors.Wrapf(err, "Cannot mount a DFS-Share with option nodfs [cmd=%s] [response=%s]", cmd, b.String())
			} else if ok && status.ExitStatus() == 32 {
				return errors.Wrapf(err, "Could not mount volume. Check parameters [cmd=%s] [response=%s]", cmd, b.String())
			}
			if ok && status.ExitStatus() != 0 {
				// The program has exited with an exit code != 0
				return errors.Wrapf(err, "Error running cmd [cmd=%s] [response=%s]", cmd, b.String())
			}
		} else {
			return errors.Wrapf(err, "Error waiting for cmd to finish [cmd=%s]", cmd)
		}
	}
	return nil
}

func createMountCmd(cmdLineArgs []string) (cmd *exec.Cmd) {
	if len(cmdLineArgs) < 4 {
		panic(retMsgInsufficientArgs)
	}

	var mArgs mounterArgs = unmarshalMounterArgs(cmdLineArgs[3])
	var optsFinal []string
	cmd = exec.Command("mount")
	cmd.Args = append(cmd.Args, "-t")
	cmd.Args = append(cmd.Args, "cifs")

	if mArgs.FsGroup != "" {
		optsFinal = append(optsFinal, fmt.Sprintf("uid=%s,gid=%s", mArgs.FsGroup, mArgs.FsGroup))
	}
	if mArgs.ReadWrite != "" {
		optsFinal = append(optsFinal, mArgs.ReadWrite)
	}
	if mArgs.CredentialDomain != "" {
		optsFinal = append(optsFinal, fmt.Sprintf("domain=%s", strings.Trim(mArgs.CredentialDomain, "\n\r")))
	}
	if mArgs.CredentialUser != "" {
		optsFinal = append(optsFinal, fmt.Sprintf("username=%s", strings.Trim(mArgs.CredentialUser, "\n\r")))
	}
	if mArgs.CredentialPass != "" {
		cmd.Env = append(os.Environ(), fmt.Sprintf("PASSWD=%s", strings.Trim(mArgs.CredentialPass, "\n\r")))
	}
	if mArgs.Opts != "" {
		optsFinal = append(optsFinal, strings.Split(mArgs.Opts, ",")...)
	} else if mArgs.MountOptions != "" {
		optsFinal = append(optsFinal, strings.Split(mArgs.MountOptions, ",")...)
	}
	if len(optsFinal) > 0 {
		cmd.Args = append(cmd.Args, "-o", strings.Join(optsFinal, ","))
	}

	if mArgs.Server != "" && mArgs.Share != "" {
		cmd.Args = append(cmd.Args, fmt.Sprintf("//%s%s", mArgs.Server, mArgs.Share))
	} else if mArgs.Source != "" {
		cmd.Args = append(cmd.Args, mArgs.Source)
	} else {
		panic(retMsgInvalidMounterArgs)
	}

	cmd.Args = append(cmd.Args, cmdLineArgs[2])

	return cmd
}

func createUmountCmd(cmdLineArgs []string) (cmd *exec.Cmd) {
	if len(cmdLineArgs) < 3 {
		panic(retMsgInsufficientArgs)
	}
	cmd = exec.Command("umount")
	cmd.Args = append(cmd.Args, cmdLineArgs[2])
	return cmd
}

// Dettach from main, allows tests to be written for this function
func driverMain(args []string) (ret returnMsg) {
	ret.Status = retStatSuccess

	defer func() {
		err := recover()
		if err != nil {
			ret.Status = retStatFailure
			ret.Message = fmt.Sprintf("Unexpected executing volume driver: %s", err)
			return
		}
	}()

	if len(args) < 2 {
		ret.Status = retStatFailure
		ret.Message = retMsgInsufficientArgs
		return
	}

	var operation = args[1]
	var err error
	switch operation {
	case "init":
		log.Println("Driver init")
		ret.Status = retStatSuccess
		ret.Capabilities.Attach = false          // this driver does not attach any devices
		ret.Capabilities.FSGroup = false         // avoids chown/chmod upstream in driver caller
		ret.Capabilities.SupportsMetrics = false // there are no metrics
	case "mount":
		cmd := createMountCmd(args)
		log.Println(cmd.Args)
		err = runCommand(cmd)
		if err != nil {
			ret.Status = retStatFailure
			ret.Message = fmt.Sprintf("Error: %s", err)
		}
	case "unmount":
		cmd := createUmountCmd(args)
		log.Println(cmd.Args)
		err = runCommand(cmd)
		if err != nil {
			ret.Status = retStatFailure
			ret.Message = fmt.Sprintf("Error: %s", err)
		}
	default:
		ret.Status = retStatNotSupported
		ret.Message = retMsgUnsupportedOperation + ": " + operation
	}
	return
}

func main() {
	// Logging to file on disk. Logfile does not contain auth information
	logfile, err := os.OpenFile(logFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Printf("WARNING: error opening file: %v", err)
	}
	log.SetOutput(logfile)

	m := driverMain(os.Args)
	jsonString, _ := json.Marshal(m)
	fmt.Println(string(jsonString))
	log.Println(string(jsonString))
	if m.Status != retStatSuccess {
		os.Exit(1)
	}
}
