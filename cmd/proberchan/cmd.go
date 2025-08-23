package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	_ "net/http/pprof"

	"github.com/felixge/fgprof"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	logLevelKey = "log-level"
	configKey   = "config"
)

func main() {
	var rootCmd = &cobra.Command{
		Use: "proberchan",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			perfListenAddr, err := cmd.Flags().GetString("perf-listen-addr")
			if err != nil {
				return err
			}
			http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
			go func() {
				fmt.Println(http.ListenAndServe(perfListenAddr, nil))
			}()
			return nil
		},
		RunE: run,
	}
	rootCmd.PersistentFlags().String("perf-listen-addr", "127.0.0.1:6060", "pprof listen address")
	rootCmd.Flags().String(logLevelKey, "debug", "log level")
	rootCmd.Flags().String(configKey, "config.yaml", "config file path")

	viper.SetEnvPrefix("PROBERCHAN")
	viper.BindPFlags(rootCmd.Flags())
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))

	if err := rootCmd.Execute(); err != nil {
		fmt.Printf("%+v\n", err)
		os.Exit(1)
	}
}
