// sbbot-normalize generates a candidate; it never reloads or changes a database.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"local/bots/internal/sbbot"
	"os"
)

func main() {
	input := flag.String("input", "", "source sing-box configuration")
	output := flag.String("output", "", "new candidate path (must not exist)")
	flag.Parse()
	if err := run(*input, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(input, output string) error {
	if input == "" || output == "" || input == output {
		return fmt.Errorf("provide distinct -input and -output paths")
	}
	d, err := sbbot.Load(input)
	if err != nil {
		return err
	}
	if err = sbbot.ValidateUnified(d); err != nil {
		return err
	}
	if err = sbbot.NormalizeDirectUsers(d); err != nil {
		return err
	}
	if err = sbbot.ValidateUnified(d); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}
