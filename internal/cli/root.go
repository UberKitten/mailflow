package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	rootCmd = &cobra.Command{
		Use:   "mailflow",
		Short: "Mailflow sorts O365 email into folders using Microsoft Graph",
	}
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		slog.Error("command failed", "error", err)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initLogging)
	rootCmd.PersistentFlags().String("config-dir", defaultConfigDir(), "config directory")

	rootCmd.AddCommand(processCmd)
	rootCmd.AddCommand(resortCmd)
	rootCmd.AddCommand(gapsCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(reloadCmd)
}

func initLogging() {
	level := new(slog.LevelVar)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func defaultConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home + "/.config/appdata/mailflow"
}
