package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	listDevices := flag.Bool("devices", false, "list input devices and exit")
	flag.Parse()

	if *listDevices {
		for _, d := range listInputDevices() {
			fmt.Println(d)
		}
		return
	}

	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
