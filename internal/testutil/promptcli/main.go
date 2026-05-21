package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	reader := bufio.NewReader(os.Stdin)
	if os.Getenv("PROMPTCLI_REPEAT_CONFIRM") == "1" {
		confirm(reader, "Drop field? [y/N]: ")
		confirm(reader, "Delete column? [y/N]: ")
		fmt.Println("confirmed twice")
		return
	}
	fmt.Print("Continue? [y/N]: ")
	confirm(reader, "")
	fmt.Print("Name: ")
	name, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "read name: %v\n", err)
		os.Exit(1)
	}
	name = strings.TrimSpace(name)
	fmt.Printf("Hello, %s\n", name)
}

func confirm(reader *bufio.Reader, prompt string) {
	if prompt != "" {
		fmt.Print(prompt)
	}
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
}
