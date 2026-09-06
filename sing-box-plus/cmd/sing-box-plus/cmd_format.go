// 本文件复制自上游 sing-box 的 cmd/sing-box/cmd_format.go。
//
// 复制而非 import：github.com/sagernet/sing-box/cmd/sing-box 是 package main，Go 禁止导入；
// 而本项目必须保持 run / check / format / version 的 argv 与退出码与上游兼容（README §4.7）。
//
// 来源：github.com/SagerNet/sing-box@0b8995879f29a9b98ee027bc17b75e101445b238（v1.14.0）
// 复制日期：2026-09-06
// 本项目修改：无（仅加本注释头）
//
// 本文件是 GPLv3 衍生物，义务见本子目录的 LICENSE 与 THIRD_PARTY_NOTICES.md。

package main

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badjson"

	"github.com/spf13/cobra"
)

var commandFormatFlagWrite bool

var commandFormat = &cobra.Command{
	Use:   "format",
	Short: "Format configuration",
	Run: func(cmd *cobra.Command, args []string) {
		err := format()
		if err != nil {
			log.Fatal(err)
		}
	},
	Args: cobra.NoArgs,
}

func init() {
	commandFormat.Flags().BoolVarP(&commandFormatFlagWrite, "write", "w", false, "write result to (source) file instead of stdout")
	mainCommand.AddCommand(commandFormat)
}

func format() error {
	optionsList, err := readConfig()
	if err != nil {
		return err
	}
	for _, optionsEntry := range optionsList {
		comments := optionsEntry.options.Comments()
		optionsEntry.options, err = badjson.Omitempty(globalCtx, optionsEntry.options)
		if err != nil {
			return err
		}
		optionsEntry.options.SetComments(comments)
		buffer := new(bytes.Buffer)
		encoder := json.NewEncoder(buffer)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(optionsEntry.options)
		if err != nil {
			return E.Cause(err, "encode config")
		}
		outputPath, _ := filepath.Abs(optionsEntry.path)
		if !commandFormatFlagWrite {
			if len(optionsList) > 1 {
				os.Stdout.WriteString(outputPath + "\n")
			}
			os.Stdout.WriteString(buffer.String() + "\n")
			continue
		}
		if bytes.Equal(optionsEntry.content, buffer.Bytes()) {
			continue
		}
		output, err := os.Create(optionsEntry.path)
		if err != nil {
			return E.Cause(err, "open output")
		}
		_, err = output.Write(buffer.Bytes())
		output.Close()
		if err != nil {
			return E.Cause(err, "write output")
		}
		os.Stderr.WriteString(outputPath + "\n")
	}
	return nil
}
