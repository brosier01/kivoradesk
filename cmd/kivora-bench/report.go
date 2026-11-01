package main

import (
	"fmt"
	"sort"
)

func printReport(r *report) {
	fmt.Printf("KivoraDesk benchmark %q, %s\n%s, %s\n\n", r.Label, r.Date, r.Version, r.Chromium)
	if b := r.Boot; b != nil {
		fmt.Printf("Boot: SSH after %.1f s, first service at %.1f s of uptime, browser at %.1f s, MCP tools after %.1f s\n",
			b.SSHSeconds, b.ServicesUptime, b.BrowserUptime, b.MCPSeconds)
	}
	fmt.Printf("Disk: %.0f MiB used on /, %d packages\n", r.Disk.RootUsedMiB, r.Disk.Packages)
	fmt.Printf("Idle: %.0f MiB used, CPU %.2f %%\n", r.Idle.UsedMiB, r.CPUIdle)
	printPSS(r.Idle)
	fmt.Printf("With %d sites open: %.0f MiB used\n", len(r.Sites), r.Loaded.UsedMiB)
	printPSS(r.Loaded)
	fmt.Printf("\n%-36s %9s %9s\n", "tool", "p50 ms", "p95 ms")
	for _, l := range r.Latency {
		fmt.Printf("%-36s %9.1f %9.1f\n", l.Name, l.P50ms, l.P95ms)
	}
}

func printPSS(m memory) {
	for _, k := range keys(m.PSSMiB) {
		fmt.Printf("  %-12s %7.1f MiB\n", k, m.PSSMiB[k])
	}
}

func printComparison(a, b *report) {
	fmt.Printf("%-36s %10s %10s %9s\n", "", a.Label, b.Label, "change")
	row := func(name string, x, y float64, unit string) {
		change := "        ="
		if x != 0 && x != y {
			change = fmt.Sprintf("%+8.1f%%", 100*(y-x)/x)
		}
		fmt.Printf("%-36s %10s %10s %s\n", name, num(x, unit), num(y, unit), change)
	}
	if a.Boot != nil && b.Boot != nil {
		row("boot: SSH", a.Boot.SSHSeconds, b.Boot.SSHSeconds, "s")
		row("boot: browser (uptime)", a.Boot.BrowserUptime, b.Boot.BrowserUptime, "s")
		row("boot: MCP tools", a.Boot.MCPSeconds, b.Boot.MCPSeconds, "s")
	}
	row("disk: / used", a.Disk.RootUsedMiB, b.Disk.RootUsedMiB, "MiB")
	row("disk: packages", float64(a.Disk.Packages), float64(b.Disk.Packages), "")
	row("idle: memory used", a.Idle.UsedMiB, b.Idle.UsedMiB, "MiB")
	for _, k := range keys(a.Idle.PSSMiB, b.Idle.PSSMiB) {
		row("idle: "+k+" (PSS)", a.Idle.PSSMiB[k], b.Idle.PSSMiB[k], "MiB")
	}
	row("idle: CPU", a.CPUIdle, b.CPUIdle, "%")
	row("sites: memory used", a.Loaded.UsedMiB, b.Loaded.UsedMiB, "MiB")
	row("sites: chromium (PSS)", a.Loaded.PSSMiB["chromium"], b.Loaded.PSSMiB["chromium"], "MiB")
	lb := map[string]latency{}
	for _, l := range b.Latency {
		lb[l.Name] = l
	}
	for _, l := range a.Latency {
		row(l.Name+" p50", l.P50ms, lb[l.Name].P50ms, "ms")
	}
}

func num(v float64, unit string) string {
	if unit == "" {
		return fmt.Sprintf("%.0f", v)
	}
	if unit == "%" {
		return fmt.Sprintf("%.2f %%", v)
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

func keys(maps ...map[string]float64) []string {
	set := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			set[k] = true
		}
	}
	var ks []string
	for k := range set {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
