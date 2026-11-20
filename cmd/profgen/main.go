// Command profgen generates a synthetic fleet of aquarium control
// programs and either uploads it to a running profiling service or prints
// a summary of the planted slowdowns.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

	"aquaprofile/internal/gen"
	"aquaprofile/internal/store"
)

func main() {
	addr := flag.String("addr", "http://localhost:8080", "profiling service base URL")
	dryRun := flag.Bool("dry-run", false, "generate and summarize without uploading")
	programs := flag.Int("programs", 30, "number of control programs")
	procs := flag.Int("procs", 3, "processes per program")
	samples := flag.Int("samples", 400, "samples per process per version")
	seed := flag.Int64("seed", 20261121, "deterministic RNG seed")
	flag.Parse()

	fleet := gen.Generate(gen.Config{
		Programs: *programs, ProcsPerProg: *procs,
		SamplesPerVer: *samples, Seed: *seed,
	})

	fmt.Printf("generated %d processes (%d programs, %d versions)\n",
		len(fleet.Procs), *programs, 2)
	fmt.Printf("planted slow functions: %v\n", fleet.SlowTargets)

	if *dryRun {
		sum := map[string]int{}
		for _, p := range fleet.Procs {
			if p.Version != "v1.0" {
				continue
			}
			for name, c := range p.Counts {
				sum[name] += c
			}
		}
		for name, c := range sum {
			fmt.Printf("  v1 %-22s %d samples\n", name, c)
		}
		return
	}

	client := &http.Client{}
	post := func(path string, body any) error {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		resp, err := client.Post(*addr+path, "application/json", bytes.NewReader(buf))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("%s -> %s", path, resp.Status)
		}
		return nil
	}

	nMod, nBatch := 0, 0
	for _, p := range fleet.Procs {
		if err := post("/api/modules", store.ModuleUpload{
			Program: p.Program, Version: p.Version, Pid: p.Pid, Modules: p.Modules,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "module upload failed:", err)
			os.Exit(1)
		}
		nMod++
	}
	for _, p := range fleet.Procs {
		if err := post("/api/samples", p.Batch); err != nil {
			fmt.Fprintln(os.Stderr, "sample upload failed:", err)
			os.Exit(1)
		}
		nBatch++
	}
	fmt.Printf("uploaded %d module tables and %d sample batches to %s\n",
		nMod, nBatch, *addr)
}
