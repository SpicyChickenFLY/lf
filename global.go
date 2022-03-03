package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// XXX: need gFilesPath only

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
