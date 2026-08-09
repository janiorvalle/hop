package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/janiorvalle/hop/internal/render"
	"golang.org/x/term"
)

func writeJSON(writer io.Writer, document glanceDocument) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return fmt.Errorf("write account list JSON: %w", err)
	}
	return nil
}

func writeTable(writer io.Writer, document glanceDocument, options render.Options) error {
	rows := make([]render.Row, 0, len(document.Accounts))
	for _, account := range document.Accounts {
		row := render.Row{
			Provider: account.Provider,
			Account:  account.Account,
			Active:   account.Active,
			Plan:     account.Plan,
			Windows:  account.Windows,
			Limits:   account.Limits,
		}
		if account.Error != nil {
			row.Problem = &render.Problem{Message: account.Error.Message, Action: account.Error.Action}
		}
		rows = append(rows, row)
	}
	return render.Table(writer, rows, options)
}

func terminalOptions(writer io.Writer, now time.Time) render.Options {
	width := 120
	_, noColor := os.LookupEnv("NO_COLOR")
	dumb := os.Getenv("TERM") == "dumb"
	isTerminal := false
	if outputFile, ok := writer.(*os.File); ok {
		fileDescriptor := int(outputFile.Fd())
		isTerminal = term.IsTerminal(fileDescriptor)
		if detectedWidth, _, err := term.GetSize(fileDescriptor); err == nil && detectedWidth > 0 {
			width = detectedWidth
		}
	}
	if columns, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && columns > 0 {
		width = columns
	}
	plain := noColor || dumb || !isTerminal
	return render.Options{Color: !plain, Plain: plain, Width: width, Now: now}
}
