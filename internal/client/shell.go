package client

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"muse-proxy/internal/proto"
)

// shellSession is one pty-backed shell attached to a stream.
type shellSession struct {
	id     uint32
	f      *forwarder
	master *os.File
	cmd    *exec.Cmd
	once   sync.Once
}

type shellSize struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

// openPty allocates a pty master/slave pair (Linux, /dev/ptmx).
func openPty() (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// unlockpt: TIOCSPTLCK is _IOW (takes a pointer), so IoctlSetPointerInt,
	// not IoctlSetInt (which passes the value as the address).
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(mfd)
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN) // ptsname number
	if err != nil {
		unix.Close(mfd)
		return nil, nil, fmt.Errorf("ptsname: %w", err)
	}
	sfd, err := unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		unix.Close(mfd)
		return nil, nil, fmt.Errorf("open slave: %w", err)
	}
	return os.NewFile(uintptr(mfd), "pty-master"), os.NewFile(uintptr(sfd), "pty-slave"), nil
}

func setWinsize(f *os.File, cols, rows uint32) {
	_ = unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}

func pickShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

// handleOpenShell spawns a pty-backed shell for the stream.
func (f *forwarder) handleOpenShell(id uint32, payload []byte) {
	if !f.allowShell {
		log.Printf("stream %d: shell disabled, refusing", id)
		f.sendFrame(proto.FrameClose, id, nil)
		return
	}
	var sz shellSize
	if err := json.Unmarshal(payload, &sz); err != nil || sz.Cols == 0 || sz.Rows == 0 {
		sz = shellSize{Cols: 80, Rows: 24}
	}
	master, slave, err := openPty()
	if err != nil {
		log.Printf("stream %d: pty: %v", id, err)
		f.sendFrame(proto.FrameClose, id, nil)
		return
	}
	setWinsize(master, sz.Cols, sz.Rows)
	shell := pickShell()
	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		// Ctty is the fd number *in the child*: the slave was dup'ed to
		// 0/1/2, so 1 (stdout) is valid there. Passing the parent's fd
		// fails with "Setctty set but Ctty not valid in child".
		Ctty: 1,
	}
	if err := cmd.Start(); err != nil {
		log.Printf("stream %d: start %s: %v", id, shell, err)
		master.Close()
		slave.Close()
		f.sendFrame(proto.FrameClose, id, nil)
		return
	}
	slave.Close() // parent keeps only the master side

	sh := &shellSession{id: id, f: f, master: master, cmd: cmd}
	f.mu.Lock()
	if _, exists := f.streams[id]; exists {
		f.mu.Unlock()
		sh.close()
		f.sendFrame(proto.FrameClose, id, nil)
		return
	}
	f.streams[id] = &stream{id: id, shell: sh}
	f.mu.Unlock()
	log.Printf("stream %d: shell started (%s %dx%d)", id, shell, sz.Cols, sz.Rows)
	go sh.pump()
	go sh.wait()
}

// pump copies pty -> websocket until EOF/error, then tears the stream down.
func (sh *shellSession) pump() {
	defer sh.f.closeStream(sh.id)
	defer sh.f.sendFrame(proto.FrameClose, sh.id, nil)
	buf := make([]byte, 32*1024)
	for {
		n, err := sh.master.Read(buf)
		if n > 0 {
			if werr := sh.f.sendFrame(proto.FrameData, sh.id, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// wait reaps the shell process; when the shell exits the stream goes down.
func (sh *shellSession) wait() {
	sh.cmd.Wait()
	sh.close()
	sh.f.closeStream(sh.id)
	sh.f.sendFrame(proto.FrameClose, sh.id, nil)
}

func (sh *shellSession) close() {
	sh.once.Do(func() {
		sh.master.Close()
		if sh.cmd.Process != nil {
			sh.cmd.Process.Kill()
		}
	})
}

func (sh *shellSession) write(p []byte) error {
	_, err := sh.master.Write(p)
	return err
}

func (sh *shellSession) winch(cols, rows uint32) {
	setWinsize(sh.master, cols, rows)
}
