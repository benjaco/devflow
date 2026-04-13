package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Continue? [y/N]: ")
	answer, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "read confirm: %v\n", err)
		os.Exit(1)
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(os.Stderr, "cancelled")
		os.Exit(2)
	}
	fmt.Print("Name: ")
	name, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "read name: %v\n", err)
		os.Exit(1)
	}
	name = strings.TrimSpace(name)
	fmt.Printf("Hello, %s\n", name)
}
