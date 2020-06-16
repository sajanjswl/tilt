package prompt

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/fatih/color"
	tty "github.com/mattn/go-tty"
	"github.com/pkg/browser"

	"github.com/tilt-dev/tilt/internal/hud"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/pkg/model"
)

type TerminalInput interface {
	ReadRune() (rune, error)
	Close() error
}

type OpenInput func() (TerminalInput, error)

type OpenURL func(url string) error

func TTYOpen() (TerminalInput, error) {
	return tty.Open()
}

func BrowserOpen(url string) error {
	return browser.OpenURL(url)
}

type TerminalPrompt struct {
	openInput  OpenInput
	openURL    OpenURL
	stdout     hud.Stdout
	host       model.WebHost
	url        model.WebURL
	printed    bool
	initOutput *bytes.Buffer
}

func NewTerminalPrompt(openInput OpenInput, openURL OpenURL, stdout hud.Stdout, host model.WebHost, url model.WebURL) *TerminalPrompt {
	return &TerminalPrompt{
		openInput: openInput,
		openURL:   openURL,
		stdout:    stdout,
		host:      host,
		url:       url,
	}
}

// Copy initial warnings and info logs from the logstore into the terminal
// prompt, so that they get shown as part of the prompt.
//
// This sits at the intersection of two incompatible interfaces:
//
// 1) The LogStore is an asynchronous, streaming log interface that makes sure
//    all logs are shown everywhere (across stdout, hud, web, snapshots, etc).
//
// 2) The TerminalPrompt is a synchronous interface that shows a deliberately
//    short "greeting" message, then blocks on user input.
//
// Rather than make these two interfaces interoperate well, we just have
// the internal/cli code copy over the logs during the init sequence.
// It's OK if logs show up twice.
func (p *TerminalPrompt) SetInitOutput(buf *bytes.Buffer) {
	p.initOutput = buf
}

func (p *TerminalPrompt) tiltBuild(st store.RStore) model.TiltBuild {
	state := st.RLockState()
	defer st.RUnlockState()
	return state.TiltBuildInfo
}

func (p *TerminalPrompt) isEnabled(st store.RStore) bool {
	state := st.RLockState()
	defer st.RUnlockState()
	return state.TerminalMode == store.TerminalModePrompt
}

func (p *TerminalPrompt) OnChange(ctx context.Context, st store.RStore) {
	if !p.isEnabled(st) {
		return
	}

	if p.printed {
		return
	}

	build := p.tiltBuild(st)
	buildStamp := build.HumanBuildStamp()
	firstLine := StartStatusLine(p.url, p.host)
	_, _ = fmt.Fprintf(p.stdout, "%s\n", firstLine)
	_, _ = fmt.Fprintf(p.stdout, "%s\n\n", buildStamp)

	// Print all the init output. See comments on SetInitOutput()
	//
	// We expect this to end in an empty newline if non-empty, which we then print
	// as a blank line. This is intended.
	infoLines := strings.Split(p.initOutput.String(), "\n")
	for _, line := range infoLines {
		if strings.HasPrefix(line, firstLine) || strings.HasPrefix(line, buildStamp) {
			continue
		}
		_, _ = fmt.Fprintf(p.stdout, "%s\n", line)
	}

	hasBrowserUI := !p.url.Empty()
	if hasBrowserUI {
		_, _ = fmt.Fprintf(p.stdout, "(space) to open the browser\n")
	}

	_, _ = fmt.Fprintf(p.stdout, "(s) to stream logs\n")
	_, _ = fmt.Fprintf(p.stdout, "(h) to open terminal HUD\n")
	_, _ = fmt.Fprintf(p.stdout, "(ctrl-c) to exit\n")

	p.printed = true

	t, err := p.openInput()
	if err != nil {
		st.Dispatch(store.ErrorAction{Error: err})
		return
	}

	keyCh := make(chan runeMessage)

	// One goroutine just pulls input from TTY.
	go func() {
		for ctx.Err() == nil {
			r, err := t.ReadRune()
			if err != nil {
				st.Dispatch(store.ErrorAction{Error: err})
				return
			}

			msg := runeMessage{
				rune:   r,
				stopCh: make(chan bool),
			}
			keyCh <- msg

			close := <-msg.stopCh
			if close {
				break
			}
		}
		close(keyCh)
	}()

	// Another goroutine processes the input. Doing this
	// on a separate goroutine allows us to clean up the TTY
	// even if it's still blocking on the ReadRune
	go func() {
		defer func() {
			_ = t.Close()
		}()

		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-keyCh:
				if !ok {
					return
				}

				r := msg.rune
				switch r {
				case 's':
					st.Dispatch(SwitchTerminalModeAction{Mode: store.TerminalModeStream})
					msg.stopCh <- true

				case 'h':
					st.Dispatch(SwitchTerminalModeAction{Mode: store.TerminalModeHUD})
					msg.stopCh <- true

				case ' ':
					_, _ = fmt.Fprintf(p.stdout, "Opening browser: %s\n", p.url.String())
					err := p.openURL(p.url.String())
					if err != nil {
						_, _ = fmt.Fprintf(p.stdout, "Error: %v\n", err)
					}
					msg.stopCh <- false
				default:
					_, _ = fmt.Fprintf(p.stdout, "Unrecognized option: %s\n", string(r))
					msg.stopCh <- false

				}
			}
		}
	}()
}

type runeMessage struct {
	rune rune

	// The receiver of this message should
	// ACK the channel when they're done.
	//
	// Sending 'true' indicates that we're switching to a different mode and the
	// input goroutine should stop reading TTY input.
	stopCh chan bool
}

func StartStatusLine(url model.WebURL, host model.WebHost) string {
	hasBrowserUI := !url.Empty()
	serverStatus := "(without browser UI)"
	if hasBrowserUI {
		if host == "0.0.0.0" {
			serverStatus = fmt.Sprintf("on %s (listening on 0.0.0.0)", url)
		} else {
			serverStatus = fmt.Sprintf("on %s", url)
		}
	}

	return color.GreenString(fmt.Sprintf("Tilt started %s", serverStatus))
}

var _ store.Subscriber = &TerminalPrompt{}
