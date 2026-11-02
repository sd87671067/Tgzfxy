package sbbot

import (
	"context"
	"golang.org/x/sys/unix"
	tg "local/bots/internal/telegram"
	"log"
	"path/filepath"
	"syscall"
	"time"
)

// Inotify only invalidates displays. It never generates or writes configuration.
func (b *Bot) WatchConfig(ctx context.Context) error {
	fd, e := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	if _, e = syscall.InotifyAddWatch(fd, filepath.Dir(b.Config.Path), syscall.IN_MOVED_TO|syscall.IN_CLOSE_WRITE); e != nil {
		return e
	}
	buf := make([]byte, 64<<10)
	for ctx.Err() == nil {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, e := unix.Poll(poll, 500)
		if e == syscall.EINTR {
			continue
		}
		if e != nil {
			return e
		}
		if n == 0 {
			continue
		}
		if _, e = syscall.Read(fd, buf); e != nil && e != syscall.EAGAIN {
			return e
		}
		b.refreshConfigDisplays(ctx)
	}
	return ctx.Err()
}
func (b *Bot) refreshConfigDisplays(ctx context.Context) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	d, e := Load(b.Config.Path)
	if e != nil {
		return
	}
	version := configVersion(d)
	for id, st := range b.States {
		if !b.Admins[id] || st.MessageID == 0 || st.ChatID == 0 || st.Version == version || time.Now().After(st.Until) {
			continue
		}
		m := tg.Message{ID: st.MessageID, From: tg.User{ID: id}, Chat: tg.Chat{ID: st.ChatID}}
		call, cancel := context.WithTimeout(ctx, 8*time.Second)
		switch st.Mode {
		case "users":
			e = b.userPage(call, m, d, st, st.Page, true)
		case "user-detail":
			if u, ok := findUser(d, st.User); ok {
				st.Version = version
				e = b.userDetail(call, m, d, u, st)
			} else {
				e = b.userPage(call, m, d, st, st.Page, true)
			}
		case "traffic":
			e = b.trafficPage(call, m, d, st, st.Action, st.Page, true)
		}
		cancel()
		if e != nil {
			log.Print("configuration display refresh failed")
		}
	}
}
