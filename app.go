package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type cmdItem struct {
	prefix string
	value  string
}

type app struct {
	ui            *ui
	nav           *nav
	ticker        *time.Ticker
	quitChan      chan struct{}
	cmd           *exec.Cmd
	cmdIn         io.WriteCloser
	cmdOutBuf     []byte
	cmdHistory    []cmdItem
	cmdHistoryBeg int
	cmdHistoryInd int
}

var gInvalidate struct {
	sort    bool
	pos     bool
	dir     bool
	navSize bool
	mouse   bool
	period  bool
}

func init() {
	gInvalidate.sort = true
	gInvalidate.pos = true
	gInvalidate.dir = true
	gInvalidate.navSize = true
	gInvalidate.mouse = true
	gInvalidate.period = true
}

func newApp(ui *ui, nav *nav) *app {
	quitChan := make(chan struct{}, 1)

	app := &app{
		ui:       ui,
		nav:      nav,
		ticker:   new(time.Ticker),
		quitChan: quitChan,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM)
	go func() {
		switch <-sigChan {
		case os.Interrupt:
			return
		case syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM:
			app.quit()
			os.Exit(3)
			return
		}
	}()

	return app
}

func (app *app) quit() {
	if err := app.writeHistory(); err != nil {
		log.Printf("writing history file: %s", err)
	}
	if !gSingleMode {
		if err := remote(fmt.Sprintf("drop %d", gClientID)); err != nil {
			log.Printf("dropping connection: %s", err)
		}
		if gOpts.autoquit {
			if err := remote("quit"); err != nil {
				log.Printf("auto quitting server: %s", err)
			}
		}
	}
}

func (app *app) readFile(path string) {
	log.Printf("reading file: %s", path)

	f, err := os.Open(path)
	if err != nil {
		app.ui.echoerrf("opening file: %s", err)
		return
	}
	defer f.Close()

	p := newParser(f)

	for p.parse() {
		p.expr.eval(app, nil)
	}

	if p.err != nil {
		app.ui.echoerrf("%s", p.err)
	}
}

func loadFiles() (list []string, cp bool, err error) {
	files, err := os.Open(gFilesPath)
	if os.IsNotExist(err) {
		err = nil
		return
	}
	if err != nil {
		err = fmt.Errorf("opening file selections file: %s", err)
		return
	}
	defer files.Close()

	s := bufio.NewScanner(files)

	s.Scan()

	switch s.Text() {
	case "copy":
		cp = true
	case "move":
		cp = false
	default:
		err = fmt.Errorf("unexpected option to copy file(s): %s", s.Text())
		return
	}

	for s.Scan() && s.Text() != "" {
		list = append(list, s.Text())
	}

	if s.Err() != nil {
		err = fmt.Errorf("scanning file list: %s", s.Err())
		return
	}

	log.Printf("loading files: %v", list)

	return
}

func saveFiles(list []string, cp bool) error {
	if err := os.MkdirAll(filepath.Dir(gFilesPath), os.ModePerm); err != nil {
		return fmt.Errorf("creating data directory: %s", err)
	}

	files, err := os.Create(gFilesPath)
	if err != nil {
		return fmt.Errorf("opening file selections file: %s", err)
	}
	defer files.Close()

	log.Printf("saving files: %v", list)

	if cp {
		fmt.Fprintln(files, "copy")
	} else {
		fmt.Fprintln(files, "move")
	}

	for _, f := range list {
		fmt.Fprintln(files, f)
	}

	return nil
}

func (app *app) readHistory() error {
	f, err := os.Open(gHistoryPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening history file: %s", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		toks := strings.SplitN(scanner.Text(), " ", 2)
		if toks[0] != ":" && toks[0] != "$" && toks[0] != "%" && toks[0] != "!" && toks[0] != "&" {
			continue
		}
		if len(toks) < 2 {
			continue
		}
		app.cmdHistory = append(app.cmdHistory, cmdItem{toks[0], toks[1]})
	}

	app.cmdHistoryBeg = len(app.cmdHistory)

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading history file: %s", err)
	}

	return nil
}

func (app *app) writeHistory() error {
	if len(app.cmdHistory) == 0 {
		return nil
	}

	local := make([]cmdItem, len(app.cmdHistory)-app.cmdHistoryBeg)
	copy(local, app.cmdHistory[app.cmdHistoryBeg:])
	app.cmdHistory = nil

	if err := app.readHistory(); err != nil {
		return fmt.Errorf("reading history file: %s", err)
	}

	app.cmdHistory = append(app.cmdHistory, local...)

	if err := os.MkdirAll(filepath.Dir(gHistoryPath), os.ModePerm); err != nil {
		return fmt.Errorf("creating data directory: %s", err)
	}

	f, err := os.Create(gHistoryPath)
	if err != nil {
		return fmt.Errorf("creating history file: %s", err)
	}
	defer f.Close()

	if len(app.cmdHistory) > 1000 {
		app.cmdHistory = app.cmdHistory[len(app.cmdHistory)-1000:]
	}

	for _, cmd := range app.cmdHistory {
		_, err = f.WriteString(fmt.Sprintf("%s %s\n", cmd.prefix, cmd.value))
		if err != nil {
			return fmt.Errorf("writing history file: %s", err)
		}
	}

	return nil
}

// This is the main event loop of the application. Expressions are read from
// the client and the server on separate goroutines and sent here over channels
// for evaluation. Similarly directories and regular files are also read in
// separate goroutines and sent here for update.
func (app *app) loop() {
	go app.nav.previewLoop(app.ui)

	var serverChan <-chan expr
	if !gSingleMode {
		serverChan = readExpr()
	}

	app.ui.readExpr()

	if gConfigPath != "" {
		if _, err := os.Stat(gConfigPath); !os.IsNotExist(err) {
			app.readFile(gConfigPath)
		} else {
			log.Printf("config file does not exist: %s", err)
		}
	} else {
		for _, path := range gConfigPaths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				app.readFile(path)
			}
		}
	}

	// config has been read, now initialize nav with the wd
	wd, err := os.Getwd()
	if err != nil {
		log.Printf("getting current directory: %s", err)
	}
	if gSelect != "" {
		_, err := os.Lstat(gSelect)
		if err != nil {
			app.ui.exprChan <- &callExpr{"echoerr", []string{err.Error()}, 1}
		} else if abs, err := filepath.Abs(gSelect); err == nil {
			// if gSelect contains the /. suffix, the directory itself
			// should be selected
			if len(gSelect) > 2 && gSelect[len(gSelect)-2:] == "/." {
				wd = abs
			} else {
				wd = filepath.Dir(abs)
				app.ui.exprChan <- &callExpr{"select", []string{abs}, 1}
			}
		}
	}
	app.nav.getDirs(wd)
	app.nav.addJumpList()

	// execute commands from args
	for _, cmd := range gCommands {
		p := newParser(strings.NewReader(cmd))

		for p.parse() {
			p.expr.eval(app, nil)
		}

		if p.err != nil {
			app.ui.echoerrf("%s", p.err)
		}
	}

	for {

		// process invalidate flags
		if gInvalidate.sort {
			app.nav.sort()
			app.ui.sort()
			gInvalidate.sort = false
		}
		if gInvalidate.pos {
			app.nav.position()
			gInvalidate.pos = false
		}
		if gInvalidate.dir {
			app.ui.loadFile(app.nav, true)
			app.ui.loadFileInfo(app.nav)
			app.ui.draw(app.nav)
			gInvalidate.dir = false
		}
		if gInvalidate.navSize {
			app.ui.renew()
			if app.nav.height != app.ui.wins[0].h {
				app.nav.height = app.ui.wins[0].h
				app.nav.regCache = make(map[string]*reg)
			}
			gInvalidate.navSize = false
		}
		if gInvalidate.mouse {
			if gOpts.mouse {
				app.ui.screen.EnableMouse()
			} else {
				app.ui.screen.DisableMouse()
			}
			gInvalidate.mouse = false
		}
		if gInvalidate.period {
			if gOpts.period == 0 {
				app.ticker.Stop()
			} else {
				app.ticker.Stop()
				app.ticker = time.NewTicker(time.Duration(gOpts.period) * time.Second)
			}
			gInvalidate.period = false
		}

		select {
		case <-app.quitChan:
			if app.nav.copyTotal > 0 {
				app.ui.echoerr("quit: copy operation in progress")
				continue
			}

			if app.nav.moveTotal > 0 {
				app.ui.echoerr("quit: move operation in progress")
				continue
			}

			if app.nav.deleteTotal > 0 {
				app.ui.echoerr("quit: delete operation in progress")
				continue
			}

			if cmd, ok := gOpts.cmds["on-quit"]; ok {
				cmd.eval(app, nil)
			}

			app.quit()

			app.nav.previewChan <- ""

			log.Print("bye!")

			if gLastDirPath != "" {
				f, err := os.Create(gLastDirPath)
				if err != nil {
					log.Printf("opening last dir file: %s", err)
				}
				defer f.Close()

				_, err = f.WriteString(app.nav.currDir().path)
				if err != nil {
					log.Printf("writing last dir file: %s", err)
				}
			}

			return
		case n := <-app.nav.copyBytesChan:
			app.nav.copyBytes += n
			// n is usually 4096B so update roughly per 4096B x 1024 = 4MB copied
			if app.nav.copyUpdate++; app.nav.copyUpdate >= 1024 {
				app.nav.copyUpdate = 0
				app.ui.draw(app.nav)
			}
		case n := <-app.nav.copyTotalChan:
			app.nav.copyTotal += n
			if n < 0 {
				app.nav.copyBytes += n
			}
			if app.nav.copyTotal == 0 {
				app.nav.copyUpdate = 0
			}
			app.ui.draw(app.nav)
		case n := <-app.nav.moveCountChan:
			app.nav.moveCount += n
			if app.nav.moveUpdate++; app.nav.moveUpdate >= 1000 {
				app.nav.moveUpdate = 0
				app.ui.draw(app.nav)
			}
		case n := <-app.nav.moveTotalChan:
			app.nav.moveTotal += n
			if n < 0 {
				app.nav.moveCount += n
			}
			if app.nav.moveTotal == 0 {
				app.nav.moveUpdate = 0
			}
			app.ui.draw(app.nav)
		case n := <-app.nav.deleteCountChan:
			app.nav.deleteCount += n
			if app.nav.deleteUpdate++; app.nav.deleteUpdate >= 1000 {
				app.nav.deleteUpdate = 0
				app.ui.draw(app.nav)
			}
		case n := <-app.nav.deleteTotalChan:
			app.nav.deleteTotal += n
			if n < 0 {
				app.nav.deleteCount += n
			}
			if app.nav.deleteTotal == 0 {
				app.nav.deleteUpdate = 0
			}
			app.ui.draw(app.nav)
		case d := <-app.nav.dirChan:

			if !app.nav.checkDir(d) {
				log.Printf("debug: dirChan skip/reload %s", d.path)
				continue
			}
			log.Printf("debug: dirChan %s %t", d.path, d.loading)

			if gOpts.dircache {
				prev, ok := app.nav.dirCache[d.path]
				if ok {
					d.ind = prev.ind
					d.sel(prev.name(), app.nav.height)
				}

				app.nav.dirCache[d.path] = d
			}

			for i := range app.nav.dirs {
				if app.nav.dirs[i].path == d.path {
					app.nav.dirs[i] = d
				}
			}

			app.nav.position()

			curr, err := app.nav.currFile()
			if err == nil {
				if d.path == app.nav.currDir().path {
					app.ui.loadFile(app.nav, true)
				}
				if d.path == curr.path {
					app.ui.dirPrev = d
				}
			}

			app.ui.draw(app.nav)
		case r := <-app.nav.regChan:
			app.nav.checkReg(r)

			app.nav.regCache[r.path] = r

			curr, err := app.nav.currFile()
			if err == nil {
				if r.path == curr.path {
					app.ui.regPrev = r
				}
			}

			app.ui.draw(app.nav)
		case ev := <-app.ui.evChan:
			e := app.ui.readEvent(ev)
			if e == nil {
				continue
			}
			e.eval(app, nil)
		loop:
			for {
				select {
				case ev := <-app.ui.evChan:
					e = app.ui.readEvent(ev)
					if e == nil {
						continue
					}
					e.eval(app, nil)
				default:
					break loop
				}
			}
			app.ui.draw(app.nav)
		case e := <-app.ui.exprChan:
			e.eval(app, nil)
			app.ui.draw(app.nav)
		case e := <-serverChan:
			e.eval(app, nil)
			app.ui.draw(app.nav)
		case <-app.ticker.C:
			app.nav.renew()
			app.ui.loadFile(app.nav, false)
			app.ui.draw(app.nav)
		}
	}
}

// This function is used to run a shell command. Modes are as follows:
//
//     Prefix  Wait  Async  Stdin  Stdout  Stderr  UI action
//     $       No    No     Yes    Yes     Yes     Pause and then resume
//     %       No    No     Yes    Yes     Yes     Statline for input/output
//     !       Yes   No     Yes    Yes     Yes     Pause and then resume
//     &       No    Yes    No     No      No      Do nothing
func (app *app) runShell(s string, args []string, prefix string) {
	app.nav.exportFiles()
	exportOpts()

	cmd := shellCommand(s, args)

	var out io.Reader
	var err error
	switch prefix {
	case "$", "!":
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		app.nav.previewChan <- ""
		if err := app.ui.suspend(); err != nil {
			log.Printf("suspend: %s", err)
		}
		defer func() {
			if err := app.ui.resume(); err != nil {
				app.quit()
				os.Exit(3)
				return
			}
		}()
		defer app.nav.renew()

		err = cmd.Run()
	case "%":
		shellSetPG(cmd)
		if app.ui.cmdPrefix == ">" {
			return
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			log.Printf("writing stdin: %s", err)
		}
		app.cmdIn = stdin
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("reading stdout: %s", err)
		}
		out = stdout
		cmd.Stderr = cmd.Stdout
		fallthrough
	case "&":
		shellSetPG(cmd)
		err = cmd.Start()
	}

	if err != nil {
		app.ui.echoerrf("running shell: %s", err)
	}

	switch prefix {
	case "!":
		anyKey()
	}

	app.ui.loadFile(app.nav, true)

	switch prefix {
	case "%":
		normal(app)
		app.cmd = cmd
		app.cmdOutBuf = nil
		app.ui.msg = ""
		app.ui.cmdPrefix = ">"

		go func() {
			eol := false
			reader := bufio.NewReader(out)
			for {
				b, err := reader.ReadByte()
				if err == io.EOF {
					break
				}
				if eol {
					eol = false
					app.cmdOutBuf = nil
				}
				app.cmdOutBuf = append(app.cmdOutBuf, b)
				if b == '\n' || b == '\r' {
					eol = true
				}
				if reader.Buffered() > 0 {
					continue
				}
				app.ui.exprChan <- &callExpr{"echo", []string{string(app.cmdOutBuf)}, 1}
			}

			if err := cmd.Wait(); err != nil {
				log.Printf("running shell: %s", err)
			}
			app.cmd = nil
			app.ui.cmdPrefix = ""
			app.ui.exprChan <- &callExpr{"load", nil, 1}
		}()
	case "&":
		go func() {
			if err := cmd.Wait(); err != nil {
				log.Printf("running shell: %s", err)
			}
		}()
	}
}
