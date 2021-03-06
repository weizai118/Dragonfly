/*
 * Copyright The Dragonfly Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package downloader

import (
	"bytes"
	"math/rand"
	"os"
	"time"

	"github.com/dragonflyoss/Dragonfly/dfget/config"
	"github.com/dragonflyoss/Dragonfly/dfget/core/api"
	"github.com/dragonflyoss/Dragonfly/dfget/core/helper"
	"github.com/dragonflyoss/Dragonfly/dfget/core/regist"
	"github.com/dragonflyoss/Dragonfly/dfget/types"
	"github.com/dragonflyoss/Dragonfly/dfget/util"
)

const (
	reset = "reset"
	last  = "last"
)

// P2PDownloader is one implementation of Downloader that uses p2p pattern
// to download files.
type P2PDownloader struct {
	Cfg            *config.Config
	API            api.SupernodeAPI
	Register       regist.SupernodeRegister
	RegisterResult *regist.RegisterResult

	node         string
	taskID       string
	targetFile   string
	taskFileName string

	pieceSizeHistory [2]int32
	queue            util.Queue
	clientQueue      util.Queue
	writerDone       chan struct{}

	clientFilePath  string
	serviceFilePath string

	// pieceSet range -> bool
	// true: if the range is processed successfully
	// false: if the range is in processing
	// not in: the range hasn't been processed
	pieceSet map[string]bool
	total    int64
}

func (p2p *P2PDownloader) init() {
	p2p.node = p2p.RegisterResult.Node
	p2p.taskID = p2p.RegisterResult.TaskID
	p2p.targetFile = p2p.Cfg.RV.RealTarget
	p2p.taskFileName = p2p.Cfg.RV.TaskFileName

	p2p.pieceSizeHistory[0], p2p.pieceSizeHistory[1] =
		p2p.RegisterResult.PieceSize, p2p.RegisterResult.PieceSize

	p2p.queue = util.NewQueue(0)
	p2p.queue.Put(NewPieceSimple(p2p.taskID, p2p.node, config.TaskStatusStart))

	p2p.clientQueue = util.NewQueue(config.DefaultClientQueueSize)
	p2p.writerDone = make(chan struct{})

	p2p.clientFilePath = helper.GetTaskFile(p2p.taskFileName, p2p.Cfg.RV.DataDir)
	p2p.serviceFilePath = helper.GetServiceFile(p2p.taskFileName, p2p.Cfg.RV.DataDir)

	p2p.pieceSet = make(map[string]bool)
}

// Run starts to download the file.
func (p2p *P2PDownloader) Run() error {
	var (
		lastItem *Piece
		goNext   bool
	)

	// start ClientWriter
	clientWriter, err := NewClientWriter(p2p.taskFileName, p2p.Cfg.RV.Cid, p2p.clientFilePath, p2p.serviceFilePath, p2p.clientQueue, p2p.Cfg)
	if err != nil {
		return err
	}
	go func() {
		clientWriter.Run()
	}()

	for {
		goNext, lastItem = p2p.getItem(lastItem)
		if !goNext {
			continue
		}
		p2p.Cfg.ClientLogger.Infof("P2P download:%v", lastItem)

		curItem := *lastItem
		curItem.Content = &bytes.Buffer{}
		lastItem = nil

		response, err := p2p.pullPieceTask(&curItem)
		if err == nil {
			code := response.Code
			if code == config.TaskCodeContinue {
				p2p.processPiece(response, &curItem)
			} else if code == config.TaskCodeFinish {
				p2p.finishTask(response, clientWriter)
				return nil
			} else {
				p2p.Cfg.ClientLogger.Warnf("Request piece result:%v", response)
				if code == config.TaskCodeSourceError {
					p2p.Cfg.BackSourceReason = config.BackSourceReasonSourceError
				}
			}
		} else {
			p2p.Cfg.ClientLogger.Errorf("P2P download fail: %v", err)
			if p2p.Cfg.BackSourceReason == 0 {
				p2p.Cfg.BackSourceReason = config.BackSourceReasonDownloadError
			}
		}

		if p2p.Cfg.BackSourceReason != 0 {
			backDownloader := NewBackDownloader(p2p.Cfg, p2p.RegisterResult)
			return backDownloader.Run()
		}
	}
}

// Cleanup clean all temporary resources generated by executing Run.
func (p2p *P2PDownloader) Cleanup() {
}

// GetNode returns supernode ip.
func (p2p *P2PDownloader) GetNode() string {
	return p2p.node
}

// GetTaskID returns downloading taskID.
func (p2p *P2PDownloader) GetTaskID() string {
	return p2p.taskID
}

func (p2p *P2PDownloader) pullPieceTask(item *Piece) (
	*types.PullPieceTaskResponse, error) {
	var (
		res *types.PullPieceTaskResponse
		err error
	)
	req := &types.PullPieceTaskRequest{
		SrcCid: p2p.Cfg.RV.Cid,
		DstCid: item.DstCid,
		Range:  item.Range,
		Result: item.Result,
		Status: item.Status,
		TaskID: item.TaskID,
	}

	for {
		if res, err = p2p.API.PullPieceTask(item.SuperNode, req); err != nil {
			p2p.Cfg.ClientLogger.Errorf("Pull piece task error: %v", err)
		} else if res.Code == config.TaskCodeWait {
			sleepTime := time.Duration(rand.Intn(1400)+600) * time.Millisecond
			p2p.Cfg.ClientLogger.Infof("Pull piece task result:%s and sleep %.3fs",
				res, sleepTime.Seconds())
			time.Sleep(sleepTime)
			continue
		}
		break
	}

	if res == nil || (res.Code != config.TaskCodeContinue &&
		res.Code != config.TaskCodeFinish &&
		res.Code != config.TaskCodeLimited &&
		res.Code != config.Success) {
		p2p.Cfg.ClientLogger.Errorf("Pull piece task fail:%v and will migrate", res)

		var registerRes *regist.RegisterResult
		if registerRes, err = p2p.Register.Register(p2p.Cfg.RV.PeerPort); err != nil {
			return nil, err
		}
		p2p.pieceSizeHistory[1] = registerRes.PieceSize
		item.Status = config.TaskStatusStart
		item.SuperNode = registerRes.Node
		item.TaskID = registerRes.TaskID
		util.Printer.Println("migrated to node:" + item.SuperNode)
		return p2p.pullPieceTask(item)
	}

	return res, err
}

func (p2p *P2PDownloader) pullRate(data *types.PullPieceTaskResponseContinueData) {

}

func (p2p *P2PDownloader) startTask(data *types.PullPieceTaskResponseContinueData) {
	powerClient := &PowerClient{
		taskID:      p2p.taskID,
		node:        p2p.node,
		pieceTask:   data,
		cfg:         p2p.Cfg,
		queue:       p2p.queue,
		clientQueue: p2p.clientQueue,
	}
	powerClient.Run()
}

func (p2p *P2PDownloader) getItem(latestItem *Piece) (bool, *Piece) {
	var (
		needMerge = true
	)
	if v, ok := p2p.queue.PollTimeout(2 * time.Second); ok {
		item := v.(*Piece)
		if item.PieceSize != 0 && item.PieceSize != p2p.pieceSizeHistory[1] {
			return false, latestItem
		}
		if item.SuperNode != p2p.node {
			item.DstCid = ""
			item.SuperNode = p2p.node
			item.TaskID = p2p.taskID
		}
		if item.Range != "" {
			v, ok := p2p.pieceSet[item.Range]
			if !ok {
				p2p.Cfg.ClientLogger.Warnf("PieceRange:%s is neither running nor success", item.Range)
				return false, latestItem
			}
			if !v && (item.Result == config.ResultSemiSuc ||
				item.Result == config.ResultSuc) {
				p2p.total += int64(item.Content.Len())
				p2p.pieceSet[item.Range] = true
			} else if !v {
				delete(p2p.pieceSet, item.Range)
			}
		}
		latestItem = item
	} else {
		p2p.Cfg.ClientLogger.Warnf("Get item timeout(2s) from queue.")
		needMerge = false
	}
	if util.IsNil(latestItem) {
		return false, latestItem
	}
	if latestItem.Result == config.ResultSuc ||
		latestItem.Result == config.ResultFail ||
		latestItem.Result == config.ResultInvalid {
		needMerge = false
	}
	runningCount := 0
	for _, v := range p2p.pieceSet {
		if !v {
			runningCount++
		}
	}
	if needMerge && (p2p.queue.Len() > 0 || runningCount > 2) {
		return false, latestItem
	}
	return true, latestItem
}

func (p2p *P2PDownloader) processPiece(response *types.PullPieceTaskResponse,
	item *Piece) {
	var (
		hasTask  = false
		sucCount = 0
	)
	p2p.refresh(item)

	data := response.ContinueData()
	for _, pieceTask := range data {
		pieceRange := pieceTask.Range
		v, ok := p2p.pieceSet[pieceRange]
		if ok && v {
			sucCount++
			p2p.queue.Put(NewPiece(p2p.taskID,
				p2p.node,
				pieceTask.Cid,
				pieceRange,
				config.ResultSemiSuc,
				config.TaskStatusRunning))
			continue
		}
		if !ok {
			p2p.pieceSet[pieceRange] = false
			p2p.pullRate(pieceTask)
			go p2p.startTask(pieceTask)
			hasTask = true
		}
	}
	if !hasTask {
		p2p.Cfg.ClientLogger.Warnf("Has not available pieceTask,maybe resource lack")
	}
	if sucCount > 0 {
		p2p.Cfg.ClientLogger.Warnf("Already suc item count:%d after a request super", sucCount)
	}
}

func (p2p *P2PDownloader) finishTask(response *types.PullPieceTaskResponse, clientWriter *ClientWriter) {
	// wait client writer finished
	p2p.Cfg.ClientLogger.Infof("Remaining writed piece count:%d", p2p.clientQueue.Len())
	p2p.clientQueue.Put(last)
	waitStart := time.Now().Unix()
	clientWriter.Wait()
	p2p.Cfg.ClientLogger.Infof("Wait client writer finish cost %d,main qu size:%d,client qu size:%d", time.Now().Unix()-waitStart, p2p.queue.Len(), p2p.clientQueue.Len())

	if p2p.Cfg.BackSourceReason > 0 {
		return
	}

	// get the temp path where the downloaded file exists.
	var src string
	if clientWriter.acrossWrite {
		src = p2p.Cfg.RV.TempTarget
	} else {
		if _, err := os.Stat(p2p.clientFilePath); err != nil {
			p2p.Cfg.ClientLogger.Infof("Client file path:%s not found", p2p.clientFilePath)
			if e := util.Link(p2p.serviceFilePath, p2p.clientFilePath); e != nil {
				p2p.Cfg.ClientLogger.Warnln("Link failed, instead of use copy")
				util.CopyFile(p2p.serviceFilePath, p2p.clientFilePath)
			}
		}
		src = p2p.clientFilePath
	}

	// move file to the target file path.
	if err := moveFile(src, p2p.targetFile, p2p.Cfg.Md5, p2p.Cfg.ClientLogger); err != nil {
		return
	}
	p2p.Cfg.ClientLogger.Infof("Download successfully from dragonfly")
}

func (p2p *P2PDownloader) refresh(item *Piece) {
	needReset := false
	if p2p.pieceSizeHistory[0] != p2p.pieceSizeHistory[1] {
		p2p.pieceSizeHistory[0] = p2p.pieceSizeHistory[1]
		needReset = true
	}

	if needReset {
		p2p.clientQueue.Put(reset)
		for k := range p2p.pieceSet {
			delete(p2p.pieceSet, k)
			p2p.total = 0
			// console log reset
		}
	}
	if p2p.node != item.SuperNode {
		p2p.node = item.SuperNode
		p2p.taskID = item.TaskID
	}
}
