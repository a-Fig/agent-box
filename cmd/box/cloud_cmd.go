package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
)

func runCloud(ctx context.Context, a *App, fs *flag.FlagSet, args []string) int {
	hostName := fs.String("host", "", "cloud host name from hosts.json")
	if parseInterspersed(fs, args) != nil || fs.NArg() != 1 {
		fmt.Fprint(a.Err, usageText())
		return 2
	}
	action := fs.Arg(0)
	switch action {
	case "pause", "resume", "stop":
	default:
		fmt.Fprintf(a.Err, "box: unknown cloud action %q (want pause, resume, or stop)\n", action)
		return 2
	}

	hosts, err := a.configuredHosts()
	if err != nil {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}
	host, err := resolveCloudHost(hosts, *hostName)
	if err != nil {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}
	ch, err := a.newCloudHost(host)
	if err != nil {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}

	switch action {
	case "pause":
		if _, err := ch.pause(ctx, true); err != nil {
			fmt.Fprintln(a.Err, "box:", err)
			return 1
		}
		fmt.Fprintf(a.Out, "paused %s\n", host.Name)
	case "resume":
		if _, err := ch.pause(ctx, false); err != nil {
			fmt.Fprintln(a.Err, "box:", err)
			return 1
		}
		fmt.Fprintf(a.Out, "resumed %s\n", host.Name)
	case "stop":
		if _, err := ch.pause(ctx, true); err != nil {
			fmt.Fprintln(a.Err, "box:", err)
			return 1
		}
		stopped, err := ch.stop(ctx)
		if err != nil {
			fmt.Fprintf(a.Err, "box: %s paused, but stop failed: %v\n", host.Name, err)
			return 1
		}
		noun := "jobs"
		if stopped == 1 {
			noun = "job"
		}
		fmt.Fprintf(a.Out, "paused %s and stopped %d %s\n", host.Name, stopped, noun)
	}
	return 0
}

func resolveCloudHost(hosts []fleetHost, name string) (fleetHost, error) {
	if name != "" {
		for _, h := range hosts {
			if h.Name != name {
				continue
			}
			if h.Kind == "" {
				return fleetHost{}, fmt.Errorf("host %q is not a cloud host", name)
			}
			return h, nil
		}
		return fleetHost{}, fmt.Errorf("unknown host %q", name)
	}

	var clouds []fleetHost
	var names []string
	for _, h := range hosts {
		if h.Kind != "" {
			clouds = append(clouds, h)
			names = append(names, h.Name)
		}
	}
	switch len(clouds) {
	case 0:
		return fleetHost{}, errors.New("no cloud host configured in hosts.json")
	case 1:
		return clouds[0], nil
	default:
		return fleetHost{}, fmt.Errorf("multiple cloud hosts configured (%s); pass --host <name>", strings.Join(names, ", "))
	}
}
