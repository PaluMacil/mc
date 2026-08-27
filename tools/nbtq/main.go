// Command nbtq pretty-prints Minecraft NBT files (gzipped or raw) as an
// indented outline, optionally filtered to matching subtrees.
//
// It exists for item-loss forensics on the mc server: player data under
// world/playerdata, and the mod-level stores under world/data, are all NBT,
// and nothing in the itzg image can read them.
//
//	nbtq world/playerdata/<uuid>.dat
//	nbtq -grep storage_uuid world/playerdata/<uuid>.dat
//	kubectl -n mc exec mc-0 -c mc -- cat /data/world/data/sophisticatedbackpacks.dat | nbtq -
//
// A -grep match prints the whole enclosing subtree, so grepping for an item id
// shows that item's full component tree rather than the one matching line.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nbtq:", err)
		os.Exit(1)
	}
}

func run() error {
	grep := flag.String("grep", "", "print only subtrees whose path or value contains this substring (case-insensitive)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nbtq [-grep SUBSTR] FILE|-")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	src := io.Reader(os.Stdin)
	if name := flag.Arg(0); name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		src = f
	}

	root, err := Parse(src)
	if err != nil {
		return err
	}
	out := os.Stdout
	defer out.Sync()
	return root.Write(out, strings.ToLower(*grep))
}
