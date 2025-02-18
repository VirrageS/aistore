// Package cli provides easy-to-use commands to manage, monitor, and utilize AIS clusters.
// This file handles commands that control running jobs in the cluster.
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmd/cli/tmpls"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/ext/etl"
	"github.com/fatih/color"
	jsoniter "github.com/json-iterator/go"
	"github.com/urfave/cli"
)

var (
	// flags
	etlSubFlags = map[string][]cli.Flag{
		subcmdCode: {
			fromFileFlag,
			depsFileFlag,
			runtimeFlag,
			commTypeFlag,
			funcTransformFlag,
			chunkSizeFlag,
			waitTimeoutFlag,
			etlUUID,
		},
		subcmdSpec: {
			fromFileFlag,
			commTypeFlag,
			etlUUID,
			waitTimeoutFlag,
		},
		subcmdStop: {
			allETLStopFlag,
		},
		subcmdBucket: {
			etlExtFlag,
			cpBckPrefixFlag,
			cpBckDryRunFlag,
			waitFlag,
			etlBucketRequestTimeout,
			templateFlag,
			listFlag,
			continueOnErrorFlag,
		},
		subcmdStart: {},
	}
	showCmdETL = cli.Command{
		Name:   commandShow,
		Usage:  "show ETL(s)",
		Action: etlListHandler,
		Subcommands: []cli.Command{
			{
				Name:      commandSource,
				Usage:     "show ETL code/spec",
				ArgsUsage: "ETL_ID",
				Action:    etlSourceHandler,
			},
		},
	}
	stopCmdETL = cli.Command{
		Name:         subcmdStop,
		Usage:        "stop ETL",
		ArgsUsage:    "[ETL_ID...]",
		Action:       etlStopHandler,
		BashComplete: etlIDCompletions,
		Flags:        etlSubFlags[subcmdStop],
	}
	startCmdETL = cli.Command{
		Name:         subcmdStart,
		Usage:        "start ETL",
		ArgsUsage:    "ETL_ID",
		Action:       etlStartHandler,
		BashComplete: etlIDCompletions,
		Flags:        etlSubFlags[subcmdStart],
	}
	initCmdETL = cli.Command{
		Name: subcmdInit,
		Subcommands: []cli.Command{
			{
				Name:   subcmdSpec,
				Usage:  "start ETL job with YAML Pod specification",
				Flags:  etlSubFlags[subcmdSpec],
				Action: etlInitSpecHandler,
			},
			{
				Name:   subcmdCode,
				Usage:  "start ETL job using the specified transforming function or script",
				Flags:  etlSubFlags[subcmdCode],
				Action: etlInitCodeHandler,
			},
		},
	}
	objCmdETL = cli.Command{
		Name:         subcmdObject,
		Usage:        "transform object",
		ArgsUsage:    "ETL_ID BUCKET/OBJECT_NAME OUTPUT",
		Action:       etlObjectHandler,
		BashComplete: etlIDCompletions,
	}
	bckCmdETL = cli.Command{
		Name:         subcmdBucket,
		Usage:        "perform bucket-to-bucket transform (\"offline transformation\")",
		ArgsUsage:    "ETL_ID SRC_BUCKET DST_BUCKET",
		Action:       etlBucketHandler,
		Flags:        etlSubFlags[subcmdBucket],
		BashComplete: manyBucketsCompletions([]cli.BashCompleteFunc{etlIDCompletions}, 1, 2),
	}
	logsCmdETL = cli.Command{
		Name:         subcmdLogs,
		Usage:        "retrieve ETL logs",
		ArgsUsage:    "ETL_ID [TARGET_ID]",
		Action:       etlLogsHandler,
		BashComplete: etlIDCompletions,
	}
	// subcommands
	etlCmd = cli.Command{
		Name:  commandETL,
		Usage: "execute custom transformations on objects",
		Subcommands: []cli.Command{
			initCmdETL,
			showCmdETL,
			logsCmdETL,
			startCmdETL,
			stopCmdETL,
			objCmdETL,
			bckCmdETL,
		},
	}
)

func etlIDCompletions(c *cli.Context) {
	suggestEtlID(c, 0)
}

func suggestEtlID(c *cli.Context, shift int) {
	if c.NArg() > shift {
		return
	}
	list, err := api.ETLList(apiBP)
	if err != nil {
		return
	}
	for _, l := range list {
		fmt.Print(l.ID)
	}
}

func etlExists(uuid string) (err error) {
	// TODO: Replace with a generic API for checking duplicate UUID
	list, err := api.ETLList(apiBP)
	if err != nil {
		return
	}
	for _, l := range list {
		if l.ID == uuid {
			return fmt.Errorf("ETL %q already exists", uuid)
		}
	}
	return
}

func etlInitSpecHandler(c *cli.Context) (err error) {
	fromFile := parseStrFlag(c, fromFileFlag)
	if fromFile == "" {
		return fmt.Errorf("%s flag cannot be empty", fromFileFlag.Name)
	}
	spec, err := os.ReadFile(fromFile)
	if err != nil {
		return err
	}

	msg := &etl.InitSpecMsg{}
	msg.IDX = parseStrFlag(c, etlUUID)
	msg.CommTypeX = parseStrFlag(c, commTypeFlag)
	msg.Spec = spec

	if err = msg.Validate(); err != nil {
		return err
	}

	// msg.ID is `metadata.name` from podSpec
	if err = etlExists(msg.ID()); err != nil {
		return
	}

	id, err := api.ETLInit(apiBP, msg)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.App.Writer, "%s\n", id)
	return nil
}

func etlInitCodeHandler(c *cli.Context) (err error) {
	msg := &etl.InitCodeMsg{}

	fromFile := parseStrFlag(c, fromFileFlag)
	if fromFile == "" {
		return fmt.Errorf("%s flag cannot be empty", fromFileFlag.Name)
	}

	msg.IDX = parseStrFlag(c, etlUUID)
	if msg.ID() != "" {
		if err = cos.ValidateEtlID(msg.ID()); err != nil {
			return
		}
		if err = etlExists(msg.ID()); err != nil {
			return
		}
	}

	if msg.Code, err = os.ReadFile(fromFile); err != nil {
		return fmt.Errorf("failed to read file: %q, err: %v", fromFile, err)
	}

	depsFile := parseStrFlag(c, depsFileFlag)
	if depsFile != "" {
		if msg.Deps, err = os.ReadFile(depsFile); err != nil {
			return fmt.Errorf("failed to read file: %q, err: %v", depsFile, err)
		}
	}

	msg.Runtime = parseStrFlag(c, runtimeFlag)
	msg.CommTypeX = parseStrFlag(c, commTypeFlag)

	if flagIsSet(c, chunkSizeFlag) {
		msg.ChunkSize, err = parseByteFlagToInt(c, chunkSizeFlag)
		if err != nil {
			return err
		}
	}

	if msg.CommTypeX != "" {
		// Missing `/` at the end, eg. `hpush:/` (should be `hpush://`)
		if strings.HasSuffix(msg.CommTypeX, ":/") {
			msg.CommTypeX += "/"
		}
		// Missing `://` at the end, eg. `hpush` (should be `hpush://`)
		if !strings.HasSuffix(msg.CommTypeX, "://") {
			msg.CommTypeX += "://"
		}
	}
	msg.Timeout = cos.Duration(parseDurationFlag(c, waitTimeoutFlag))

	// funcs
	msg.Funcs.Transform = parseStrFlag(c, funcTransformFlag)

	// validate
	if err := msg.Validate(); err != nil {
		return err
	}

	// start
	id, err := api.ETLInit(apiBP, msg)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.App.Writer, "%s\n", id)
	return nil
}

func etlListHandler(c *cli.Context) (err error) {
	list, err := api.ETLList(apiBP)
	if err != nil {
		return err
	}
	return tmpls.Print(list, c.App.Writer, tmpls.TransformListTmpl, nil, false)
}

func etlSourceHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	}
	id := c.Args().Get(0)
	msg, err := api.ETLGetInitMsg(apiBP, id)
	if err != nil {
		return err
	}
	if initMsg, ok := msg.(*etl.InitCodeMsg); ok {
		fmt.Fprintln(c.App.Writer, string(initMsg.Code))
		return
	}
	if initMsg, ok := msg.(*etl.InitSpecMsg); ok {
		fmt.Fprintln(c.App.Writer, string(initMsg.Spec))
	}
	return
}

func etlLogsHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	}

	var (
		id       = c.Args().Get(0)
		targetID = c.Args().Get(1) // optional
	)

	logs, err := api.ETLLogs(apiBP, id, targetID)
	if err != nil {
		return err
	}

	if targetID != "" {
		fmt.Fprintln(c.App.Writer, string(logs[0].Logs))
		return nil
	}

	for idx, log := range logs {
		if idx > 0 {
			fmt.Fprintln(c.App.Writer)
		}
		fmt.Fprintf(c.App.Writer, "%s:\n%s\n", log.TargetID, string(log.Logs))
	}

	return nil
}

func etlStopHandler(c *cli.Context) error {
	return stopETLs(c, "")
}

func stopETLs(c *cli.Context, id string) (err error) {
	var etlIDs []string
	switch {
	case id != "":
		etlIDs = append(etlIDs, id)
	case flagIsSet(c, allETLStopFlag):
		if c.NArg() > 0 {
			return incorrectUsageMsg(c, "'--%s' flag cannot be used together with specific ETL IDs", allETLStopFlag.Name)
		}

		res, err := api.ETLList(apiBP)
		if err != nil {
			return err
		}
		for _, etlInfo := range res {
			etlIDs = append(etlIDs, etlInfo.ID)
		}
	default:
		if c.NArg() == 0 {
			return incorrectUsageMsg(c, "specify either '--%s' flag or specific ETL ID(s)", allETLStopFlag.Name)
		}
		etlIDs = c.Args()[0:]
	}
	for _, id := range etlIDs {
		msg := fmt.Sprintf("ETL[%s]", id)
		if err := api.ETLStop(apiBP, id); err != nil {
			if herr, ok := err.(*cmn.ErrHTTP); ok && herr.Status == http.StatusNotFound {
				actionWarn(c, msg+" not found, nothing to do")
				continue
			}
			return err
		}
		actionDone(c, msg+" stopped")
	}
	return nil
}

func etlStartHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	}
	etlID := c.Args()[0]
	if err := api.ETLStart(apiBP, etlID); err != nil {
		if herr, ok := err.(*cmn.ErrHTTP); ok && herr.Status == http.StatusNotFound {
			color.New(color.FgYellow).Fprintf(c.App.Writer, "ETL %q not found", etlID)
		}
		return err
	}
	fmt.Fprintf(c.App.Writer, "ETL %q started successfully\n", etlID)
	return nil
}

func etlObjectHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	} else if c.NArg() == 1 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	} else if c.NArg() == 2 {
		return missingArgumentsError(c, "OUTPUT")
	}

	var (
		id         = c.Args()[0]
		uri        = c.Args()[1]
		outputDest = c.Args()[2]
	)

	bck, objName, err := parseBckObjectURI(c, uri)
	if err != nil {
		return err
	}

	var w io.Writer
	if outputDest == "-" {
		w = os.Stdout
	} else {
		f, err := os.Create(outputDest)
		if err != nil {
			return err
		}
		w = f
		defer f.Close()
	}

	return handleETLHTTPError(api.ETLObject(apiBP, id, bck, objName, w), id)
}

func etlBucketHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	} else if c.NArg() == 1 {
		return missingArgumentsError(c, "BUCKET_FROM")
	} else if c.NArg() == 2 {
		return missingArgumentsError(c, "BUCKET_TO")
	}

	id := c.Args()[0]

	fromBck, err := parseBckURI(c, c.Args()[1], true /*require provider*/)
	if err != nil {
		return err
	}
	toBck, err := parseBckURI(c, c.Args()[2], true /*require provider*/)
	if err != nil {
		return err
	}

	if fromBck.Equal(&toBck) {
		return fmt.Errorf("cannot ETL bucket %q onto itself", fromBck)
	}

	msg := &apc.TCBMsg{
		ID: id,
		CopyBckMsg: apc.CopyBckMsg{
			Prefix: parseStrFlag(c, cpBckPrefixFlag),
			DryRun: flagIsSet(c, cpBckDryRunFlag),
		},
	}

	if flagIsSet(c, etlExtFlag) {
		mapStr := parseStrFlag(c, etlExtFlag)
		extMap := make(cos.StrKVs, 1)
		if err = jsoniter.UnmarshalFromString(mapStr, &extMap); err != nil {
			// add quotation marks and reparse
			tmp := strings.ReplaceAll(mapStr, " ", "")
			tmp = strings.ReplaceAll(tmp, "{", "{\"")
			tmp = strings.ReplaceAll(tmp, "}", "\"}")
			tmp = strings.ReplaceAll(tmp, ":", "\":\"")
			tmp = strings.ReplaceAll(tmp, ",", "\",\"")
			if jsoniter.UnmarshalFromString(tmp, &extMap) == nil {
				err = nil
			}
		}
		if err != nil {
			return fmt.Errorf("Invalid format --%s=%q. Usage examples: {jpg:txt}, \"{in1:out1,in2:out2}\"",
				etlExtFlag.GetName(), mapStr)
		}
		msg.Ext = extMap
	}

	tmplObjs := parseStrFlag(c, templateFlag)
	listObjs := parseStrFlag(c, listFlag)
	if listObjs != "" || tmplObjs != "" {
		return multiObjBckCopy(c, fromBck, toBck, listObjs, tmplObjs, id)
	}

	xactID, err := api.ETLBucket(apiBP, fromBck, toBck, msg)
	if err := handleETLHTTPError(err, id); err != nil {
		return err
	}

	if !flagIsSet(c, waitFlag) {
		fmt.Fprintln(c.App.Writer, xactID)
		return nil
	}

	if _, err := api.WaitForXactionIC(apiBP, api.XactReqArgs{ID: xactID}); err != nil {
		return err
	}
	if !flagIsSet(c, cpBckDryRunFlag) {
		return nil
	}

	snaps, err := api.QueryXactionSnaps(apiBP, api.XactReqArgs{ID: xactID})
	if err != nil {
		return err
	}

	fmt.Fprintln(c.App.Writer, dryRunHeader+" "+dryRunExplanation)

	locObjs, outObjs, inObjs := snaps.ObjCounts(xactID)
	fmt.Fprintf(c.App.Writer, "ETL object snaps: locally transformed=%d, sent=%d, received=%d", locObjs, outObjs, inObjs)
	locBytes, outBytes, inBytes := snaps.ByteCounts(xactID)
	fmt.Fprintf(c.App.Writer, "ETL byte snaps: locally transformed=%d, sent=%d, received=%d", locBytes, outBytes, inBytes)
	return nil
}

func handleETLHTTPError(err error, etlID string) error {
	if herr, ok := err.(*cmn.ErrHTTP); ok {
		// TODO: How to find out if it's transformation not found, and not object not found?
		if herr.Status == http.StatusNotFound && strings.Contains(herr.Error(), etlID) {
			return fmt.Errorf("ETL %q not found; try starting new ETL with:\nais %s %s <spec>", etlID, commandETL, subcmdInit)
		}
	}
	return err
}
