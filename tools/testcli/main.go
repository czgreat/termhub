// Command testcli stands in for a real terminal CLI (claude, codex) in tests,
// so that tests need no account and no network. Its behaviour is driven by
// flags and by commands typed on stdin. See docs/M12 第 4 节.
package main

import (
	"bufio"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var modes = map[string]string{
	"paste":  "?2004",
	"focus":  "?1004",
	"mouse":  "?1000",
	"sgr":    "?1006",
	"altscr": "?1049",
	"ckm":    "?1",
	"wheel":  "?1007",
	"cursor": "?25",
}

func main() {
	var (
		seq         = flag.Int("seq", 0, "print this many numbered lines at start (0 = none)")
		rate        = flag.Int("rate", 0, "lines per second for -seq and flood (0 = unthrottled)")
		lineLen     = flag.Int("len", 0, "pad numbered lines to this many bytes")
		setModes    = flag.String("modes", "", "comma separated terminal modes to enable at start: "+modeNames())
		title       = flag.String("title", "", "set the terminal title at start")
		spawn       = flag.Int("spawn", 0, "spawn a child chain this deep, each sleeping for -child-life")
		childLife   = flag.Duration("child-life", time.Hour, "how long spawned descendants stay alive")
		exitAfter   = flag.Duration("exit-after", 0, "exit by itself after this long (0 = never)")
		exitCode    = flag.Int("exit-code", 0, "exit code used by -exit-after and the quit command")
		ignoreClose = flag.Bool("ignore-close", false, "ignore interrupt and close events")
		role        = flag.String("role", "", "internal: child")
	)
	flag.Parse()

	if *role == "child" {
		runChild(*spawn, *childLife)
		return
	}
	if *ignoreClose {
		// On Windows, CTRL_CLOSE_EVENT is delivered as SIGTERM.
		c := make(chan os.Signal, 8)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			for s := range c {
				fmt.Printf("IGNORED %v\r\n", s)
			}
		}()
	}

	out := bufio.NewWriterSize(os.Stdout, 64*1024)
	defer out.Flush()

	if *title != "" {
		fmt.Fprintf(out, "\x1b]0;%s\x07", *title)
	}
	for _, m := range strings.Split(*setModes, ",") {
		if code, ok := modes[strings.TrimSpace(m)]; ok {
			fmt.Fprintf(out, "\x1b[%sh", code)
		}
	}
	if *spawn > 0 {
		pid, err := spawnChild(*spawn, *childLife)
		fmt.Fprintf(out, "SPAWNED %d %v\r\n", pid, err)
	}
	for i, a := range flag.Args() { // what this process really received, for quoting tests
		fmt.Fprintf(out, "ARG %d=%q\r\n", i, a)
	}
	fmt.Fprint(out, "READY\r\n")
	out.Flush()

	if *seq > 0 {
		printSeq(out, 0, *seq, *rate, *lineLen)
	}
	if *exitAfter > 0 {
		time.AfterFunc(*exitAfter, func() { out.Flush(); os.Exit(*exitCode) })
	}

	// Command loop. Input arrives through a terminal, so lines end with CR or LF.
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := readLine(in)
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "echo": // echo <text>: report exactly the bytes received
			fmt.Fprintf(out, "ECHO %q\r\n", strings.TrimPrefix(line, "echo "))
		case "say": // say <a>|<b>|…: print the parts as consecutive raw lines (menus for the front-end tests)
			for _, part := range strings.Split(strings.TrimPrefix(line, "say "), "|") {
				fmt.Fprintf(out, "%s\r\n", part)
			}
		case "seq": // seq <from> <count>
			from, n := atoi(f, 1), atoi(f, 2)
			printSeq(out, from, n, *rate, *lineLen)
		case "sink": // sink <bytes>: read exactly that many raw bytes, report their SHA-256
			// sink <bytes> [file]: with a file, what arrived is also kept for a byte-wise comparison
			n := atoi(f, 1)
			restore := rawInput()
			sum := sha256.New()
			var dst io.Writer = sum
			if len(f) > 2 {
				if keep, err := os.Create(f[2]); err == nil {
					defer keep.Close()
					dst = io.MultiWriter(sum, keep)
				}
			}
			got, err := io.CopyN(dst, in, int64(n))
			restore()
			fmt.Fprintf(out, "SINK %d %x %v\r\n", got, sum.Sum(nil), err)
		case "flood": // flood <megabytes>
			flood(out, atoi(f, 1))
		case "mode": // mode <name> on|off
			if code, ok := modes[arg(f, 1)]; ok {
				c := "l"
				if arg(f, 2) == "on" {
					c = "h"
				}
				fmt.Fprintf(out, "\x1b[%s%s", code, c)
			}
		case "title":
			fmt.Fprintf(out, "\x1b]0;%s\x07", strings.TrimPrefix(line, "title "))
		case "size": // ask the terminal layer nothing; just print what we were told
			fmt.Fprintf(out, "SIZE %s\r\n", os.Getenv("COLUMNS")+"x"+os.Getenv("LINES"))
		case "quit":
			code := *exitCode
			if len(f) > 1 {
				code = atoi(f, 1)
			}
			out.Flush()
			os.Exit(code)
		default:
			fmt.Fprintf(out, "UNKNOWN %q\r\n", f[0])
		}
		out.Flush()
	}
}

func modeNames() string {
	var s []string
	for k := range modes {
		s = append(s, k)
	}
	return strings.Join(s, ",")
}

func readLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			return b.String(), err
		}
		if c == '\r' || c == '\n' {
			// In line mode the console turns Enter into CR LF, delivered together.
			// The LF belongs to this line, not to whatever is read next.
			if c == '\r' && r.Buffered() > 0 {
				if next, _ := r.Peek(1); len(next) == 1 && next[0] == '\n' {
					r.ReadByte()
				}
			}
			return b.String(), nil
		}
		b.WriteByte(c)
	}
}

func arg(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

func atoi(f []string, i int) int {
	n, _ := strconv.Atoi(arg(f, i))
	return n
}

// printSeq writes lines "SEQ 00000042" so a reader can verify nothing was lost or repeated.
func printSeq(out *bufio.Writer, from, n, rate, lineLen int) {
	var tick <-chan time.Time
	if rate > 0 {
		t := time.NewTicker(time.Second / time.Duration(rate))
		defer t.Stop()
		tick = t.C
	}
	for i := from; i < from+n; i++ {
		s := fmt.Sprintf("SEQ %08d", i)
		if pad := lineLen - len(s) - 2; pad > 0 {
			s += " " + strings.Repeat("x", pad-1)
		}
		out.WriteString(s + "\r\n")
		if tick != nil {
			out.Flush()
			<-tick
		}
	}
	out.Flush()
}

func flood(out *bufio.Writer, mb int) {
	line := strings.Repeat("F", 1022) + "\r\n"
	for i := 0; i < mb*1024; i++ {
		out.WriteString(line)
	}
	fmt.Fprintf(out, "FLOOD DONE %d\r\n", mb)
	out.Flush()
}

func spawnChild(depth int, life time.Duration) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(self, "-role", "child", "-spawn", strconv.Itoa(depth-1), "-child-life", life.String())
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// runChild spawns the rest of the chain, then the middle links exit so the
// last one is orphaned: the case the cleanup code must still find.
func runChild(depth int, life time.Duration) {
	if depth > 0 {
		spawnChild(depth, life)
		time.Sleep(500 * time.Millisecond)
		return
	}
	time.Sleep(life)
}
