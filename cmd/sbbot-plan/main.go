// sbbot-plan writes a private candidate only. It never installs config/restarts services.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"local/bots/internal/sbbot"
	"os"
)

func main() {
	source := flag.String("config", "", "source config")
	dest := flag.String("output", "", "new private candidate file (must not exist)")
	template := flag.String("template", "", "legacy VLESS REALITY template tag")
	tag := flag.String("tag", "sbbot-unified-main", "new listener tag")
	listen := flag.String("listen", "127.0.0.1", "candidate listen address")
	mapping := flag.String("mapping-output", "", "optional NEW private UUID migration proposal file")
	port := flag.Int("port", 18443, "candidate port; no host bind performed")
	flag.Parse()
	if err := run(*source, *dest, *template, *tag, *listen, *port, *mapping); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(source, dest, template, tag, listen string, port int, mapping string) error {
	d, e := sbbot.Load(source)
	if e != nil {
		return e
	}
	if mapping != "" {
		plan, err := sbbot.PlanMigration(d)
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(mapping, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(raw)
		ce := f.Close()
		if err != nil {
			return err
		}
		if ce != nil {
			return ce
		}
	}
	d, e = sbbot.StageUnified(d, template, tag, listen, port)
	if e != nil {
		return e
	}
	if e = sbbot.ValidateUnified(d); e != nil {
		return e
	}
	raw, e := json.MarshalIndent(d, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(append(raw, '\n'))
	if e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	return f.Close()
}
