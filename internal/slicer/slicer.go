package slicer

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/percona/percona-backup-mongodb/internal/backup"
	"github.com/percona/percona-backup-mongodb/internal/compress"
	"github.com/percona/percona-backup-mongodb/internal/config"
	"github.com/percona/percona-backup-mongodb/internal/connect"
	"github.com/percona/percona-backup-mongodb/internal/ctrl"
	"github.com/percona/percona-backup-mongodb/internal/defs"
	"github.com/percona/percona-backup-mongodb/internal/errors"
	"github.com/percona/percona-backup-mongodb/internal/lock"
	"github.com/percona/percona-backup-mongodb/internal/log"
	"github.com/percona/percona-backup-mongodb/internal/oplog"
	"github.com/percona/percona-backup-mongodb/internal/restore"
	"github.com/percona/percona-backup-mongodb/internal/storage"
	"github.com/percona/percona-backup-mongodb/internal/topo"
	"github.com/percona/percona-backup-mongodb/internal/util"
)

// Slicer is an incremental backup object
type Slicer struct {
	leadClient connect.Client
	node       *mongo.Client
	rs         string
	span       int64
	lastTS     primitive.Timestamp
	storage    storage.Storage
	oplog      *oplog.OplogBackup
	l          log.LogEvent
	ep         config.Epoch
}

// NewSlicer creates an incremental backup object
func NewSlicer(
	rs string,
	cn connect.Client,
	node *mongo.Client,
	to storage.Storage,
	ep config.Epoch,
	logger log.Logger,
) *Slicer {
	return &Slicer{
		leadClient: cn,
		node:       node,
		rs:         rs,
		span:       int64(defs.PITRdefaultSpan),
		storage:    to,
		oplog:      oplog.NewOplogBackup(node),
		l:          logger.NewEvent(string(ctrl.CmdPITR), "", "", ep.TS()),
		ep:         ep,
	}
}

// SetSpan sets span duration. Streaming will recognize the change and adjust on the next iteration.
func (s *Slicer) SetSpan(d time.Duration) {
	atomic.StoreInt64(&s.span, int64(d))
}

func (s *Slicer) GetSpan() time.Duration {
	return time.Duration(atomic.LoadInt64(&s.span))
}

// Catchup seeks for the last saved (backed up) TS - the starting point. It should be run only
// if the timeline was lost (e.g. on (re)start, restart after backup, node's fail).
// The starting point sets to the last backup's or last PITR chunk's TS whichever is the most recent.
// If there is a chunk behind the last backup it will try to fill the gaps from the chunk to the starting point.
// While filling gaps it checks the oplog for sufficiency. It also checks if there is no restore intercepted
// the timeline (hence there are no restores after the most recent backup)
func (s *Slicer) Catchup(ctx context.Context) error {
	s.l.Debug("start_catchup")
	baseBcp, err := backup.GetLastBackup(ctx, s.leadClient, nil)
	if errors.Is(err, errors.ErrNotFound) {
		return errors.New("no backup found. full backup is required to start PITR")
	}
	if err != nil {
		return errors.Wrap(err, "get last backup")
	}

	defer func() {
		s.l.Debug("lastTS set to %v %s", s.lastTS, formatts(s.lastTS))
	}()

	rstr, err := restore.GetLastRestore(ctx, s.leadClient)
	if err != nil && !errors.Is(err, errors.ErrNotFound) {
		return errors.Wrap(err, "get last restore")
	}
	if rstr != nil && rstr.StartTS > baseBcp.StartTS {
		return errors.Errorf("no backup found after the restored %s, a new backup is required to resume PITR", rstr.Backup)
	}

	chnk, err := oplog.PITRLastChunkMeta(ctx, s.leadClient, s.rs)
	if err != nil && !errors.Is(err, errors.ErrNotFound) {
		return errors.Wrap(err, "get last slice")
	}

	s.lastTS = baseBcp.LastWriteTS

	if chnk == nil {
		return nil
	}

	// PITR chunk after the recent backup is the most recent oplog slice
	if chnk.EndTS.Compare(baseBcp.LastWriteTS) >= 0 {
		s.lastTS = chnk.EndTS
		return nil
	}

	if rstr != nil && rstr.StartTS > int64(chnk.StartTS.T) {
		s.l.Info("restore `%s` is after the chunk `%s`, skip", rstr.Backup, chnk.FName)
		return nil
	}

	bl, err := backup.BackupsDoneList(ctx, s.leadClient, &chnk.EndTS, 0, -1)
	if err != nil {
		return errors.Wrapf(err, "get backups list from %v", chnk.EndTS)
	}

	if len(bl) > 1 {
		s.l.Debug("chunk too far (more than a one snapshot)")
		return nil
	}

	// if there is a gap between chunk and the backup - fill it
	// failed gap shouldn't prevent further chunk creation
	if chnk.EndTS.Compare(baseBcp.FirstWriteTS) < 0 {
		ok, err := s.oplog.IsSufficient(chnk.EndTS)
		if err != nil {
			s.l.Warning("check oplog sufficiency for %s: %v", chnk, err)
			return nil
		}
		if !ok {
			s.l.Info("insufficient range since %v", chnk.EndTS)
			return nil
		}

		cfg, err := config.GetConfig(ctx, s.leadClient)
		if err != nil {
			return errors.Wrap(err, "get config")
		}

		err = s.upload(ctx, chnk.EndTS, baseBcp.FirstWriteTS, cfg.PITR.Compression, cfg.PITR.CompressionLevel)
		if err != nil {
			s.l.Warning("create last_chunk<->sanpshot slice: %v", err)
			// duplicate key means chunk is already created by probably another routine
			// so we're safe to continue
			if !mongo.IsDuplicateKeyError(err) {
				return nil
			}
		} else {
			s.l.Info("created chunk %s - %s", formatts(chnk.EndTS), formatts(baseBcp.FirstWriteTS))
		}
	}

	if baseBcp.Type != defs.LogicalBackup || util.IsSelective(baseBcp.Namespaces) {
		// the backup does not contain complete oplog to copy from
		// NOTE: the chunk' last op can be later than backup' first write ts
		s.lastTS = chnk.EndTS
		return nil
	}

	err = s.copyFromBcp(ctx, baseBcp)
	if err != nil {
		s.l.Warning("copy snapshot [%s] oplog: %v", baseBcp.Name, err)
	} else {
		s.l.Info("copied chunk %s - %s", formatts(baseBcp.FirstWriteTS), formatts(baseBcp.LastWriteTS))
	}

	return nil
}

//nolint:nonamedreturns
func (s *Slicer) OplogOnlyCatchup(ctx context.Context) (err error) {
	s.l.Debug("start_catchup [oplog only]")

	defer func() {
		if err == nil {
			s.l.Debug("lastTS set to %v %s", s.lastTS, formatts(s.lastTS))
		}
	}()

	chnk, err := oplog.PITRLastChunkMeta(ctx, s.leadClient, s.rs)
	if err != nil && !errors.Is(err, errors.ErrNotFound) {
		return errors.Wrap(err, "get last slice")
	}

	if chnk != nil {
		ok, err := s.oplog.IsSufficient(chnk.EndTS)
		if err != nil {
			s.l.Warning("check oplog sufficiency for %s: %v", chnk, err)
			return nil
		}

		if ok {
			s.lastTS = chnk.EndTS
			return nil
		}

		s.l.Info("insufficient range since %v", chnk.EndTS)
	}

	ts, err := topo.GetClusterTime(ctx, s.leadClient)
	if err != nil {
		return err
	}

	s.lastTS = ts
	return nil
}

func (s *Slicer) copyFromBcp(ctx context.Context, bcp *backup.BackupMeta) error {
	var oplogName string
	for _, r := range bcp.Replsets {
		if r.Name == s.rs {
			oplogName = r.OplogName
			break
		}
	}
	if oplogName == "" {
		return errors.New("no data for shard")
	}

	n := s.chunkPath(bcp.FirstWriteTS, bcp.LastWriteTS, bcp.Compression)
	err := s.storage.Copy(oplogName, n)
	if err != nil {
		return errors.Wrap(err, "storage copy")
	}
	stat, err := s.storage.FileStat(n)
	if err != nil {
		return errors.Wrap(err, "file stat")
	}

	meta := oplog.OplogChunk{
		RS:          s.rs,
		FName:       n,
		Compression: bcp.Compression,
		StartTS:     bcp.FirstWriteTS,
		EndTS:       bcp.LastWriteTS,
		Size:        stat.Size,
	}
	err = oplog.PITRAddChunk(ctx, s.leadClient, meta)
	if err != nil {
		return errors.Wrapf(err, "unable to save chunk meta %v", meta)
	}

	return nil
}

// OpMovedError is the error signaling that slicing op
// now being run by the other node
type OpMovedError struct {
	to string
}

func (e OpMovedError) Error() string {
	return fmt.Sprintf("pitr slicing resumed on node %s", e.to)
}

func (e OpMovedError) Is(err error) bool {
	if err == nil {
		return false
	}

	_, ok := err.(OpMovedError) //nolint:errorlint
	return ok
}

// LogStartMsg message to log on successful streaming start
const LogStartMsg = "start_ok"

// Stream streaming (saving) chunks of the oplog to the given storage
func (s *Slicer) Stream(
	ctx context.Context,
	stopC <-chan struct{},
	backupSig <-chan *ctrl.OPID,
	compression compress.CompressionType,
	level *int, timeouts *config.BackupTimeouts,
) error {
	if s.lastTS.T == 0 {
		return errors.New("no starting point defined")
	}
	s.l.Info("streaming started from %v / %v", time.Unix(int64(s.lastTS.T), 0).UTC(), s.lastTS.T)

	cspan := s.GetSpan()
	tk := time.NewTicker(cspan)
	defer tk.Stop()

	nodeInfo, err := topo.GetNodeInfoExt(ctx, s.node)
	if err != nil {
		return errors.Wrap(err, "get NodeInfo data")
	}

	// early check for the log sufficiency to display error
	// before the timer clicks (not to wait minutes to report)
	ok, err := s.oplog.IsSufficient(s.lastTS)
	if err != nil {
		return errors.Wrap(err, "check oplog sufficiency")
	}
	if !ok {
		return oplog.InsuffRangeError{s.lastTS}
	}
	s.l.Debug(LogStartMsg)

	lastSlice := false
	llock := &lock.LockHeader{Replset: s.rs}

	var sliceTo primitive.Timestamp
	for {
		// waiting for a trigger
		select {
		// wrapping up at the current point-in-time
		// upload the chunks up to the current time and return
		case <-stopC:
			s.l.Info("got done signal, stopping")
			lastSlice = true
		// on wakeup or tick whatever comes first do the job
		case bcp := <-backupSig:
			s.l.Info("got wake_up signal")
			if bcp != nil {
				s.l.Info("wake_up for bcp %s", bcp.String())
				sliceTo, err = s.backupStartTS(ctx, bcp.String(), timeouts.StartingStatus())
				if err != nil {
					return errors.Wrap(err, "get backup start TS")
				}

				// it can happen that prevoius slice >= backup's fisrt_write
				// in that case we have to just back off.
				if s.lastTS.Compare(sliceTo) >= 0 {
					s.l.Info("pausing/stopping with last_ts %v", time.Unix(int64(s.lastTS.T), 0).UTC())
					return nil
				}
				lastSlice = true
			}
		case <-tk.C:
		}

		nextChunkT := time.Now().Add(cspan)

		// check if the node is still any good to make backups
		ninf, err := topo.GetNodeInfoExt(ctx, s.node)
		if err != nil {
			return errors.Wrap(err, "get node info")
		}
		q, err := topo.NodeSuits(ctx, s.node, ninf)
		if err != nil {
			return errors.Wrap(err, "node check")
		}
		if !q {
			return nil
		}

		// before any action check if we still got a lock. if no:
		//
		// - if there is another lock for a backup operation and we've got a
		//   `backupSig`- wait for the backup to start, make the last slice up
		//   unlit backup StartTS and return;
		// - if there is no other lock, we have to wait for the snapshot backup - see above
		//   (snapshot cmd can delete pitr lock but might not yet acquire the own one);
		// - if there is another lock and it is for pitr - return, probably split happened
		//   and a new worker was elected;
		// - any other case (including no lock) is the undefined behavior - return.
		//
		ld, err := s.getOpLock(ctx, llock, timeouts.StartingStatus())
		if err != nil {
			return errors.Wrap(err, "check lock")
		}

		// in case there is a lock, even a legit one (our own, or backup's one) but it is stale
		// we should return so the slicer would get through the lock acquisition again.
		ts, err := topo.GetClusterTime(ctx, s.leadClient)
		if err != nil {
			return errors.Wrap(err, "read cluster time")
		}
		if ld.Heartbeat.T+defs.StaleFrameSec < ts.T {
			return errors.Errorf("stale lock %#v, last beat ts: %d", ld.LockHeader, ld.Heartbeat.T)
		}

		switch ld.Type {
		case ctrl.CmdPITR:
			if ld.Node != nodeInfo.Me {
				return OpMovedError{ld.Node}
			}
			sliceTo, err = s.oplog.LastWrite(ctx)
			if err != nil {
				return errors.Wrap(err, "define last write timestamp")
			}
		case ctrl.CmdUndefined:
			return errors.New("undefined behavior operation is running")
		case ctrl.CmdBackup:
			// continue only if we had `backupSig`
			if !lastSlice || s.lastTS.Compare(sliceTo) == 0 {
				return errors.Errorf("another operation is running: %#v", ld)
			}
		default:
			return errors.Errorf("another operation is running: %#v", ld)
		}

		// if this is the last slice, epoch probably already changed (e.g. due to config changes) and that's ok
		if !lastSlice {
			cep, err := config.GetEpoch(ctx, s.leadClient)
			if err != nil {
				return errors.Wrap(err, "get epoch")
			}
			if s.ep.TS().Compare(cep.TS()) != 0 {
				return errors.Errorf("epoch mismatch. Got sleep in %v, woke up in %v. Too old for that stuff.", s.ep.TS(), cep.TS())
			}
		}

		err = s.upload(ctx, s.lastTS, sliceTo, compression, level)
		if err != nil {
			return err
		}

		logm := fmt.Sprintf("created chunk %s - %s", formatts(s.lastTS), formatts(sliceTo))
		if !lastSlice {
			logm += fmt.Sprintf(". Next chunk creation scheduled to begin at ~%s", nextChunkT.Format("2006-01-02T15:04:05"))
		}
		s.l.Info(logm)

		if lastSlice {
			s.l.Info("pausing/stopping with last_ts %v", time.Unix(int64(sliceTo.T), 0).UTC())
			return nil
		}

		s.lastTS = sliceTo

		if ispan := s.GetSpan(); cspan != ispan {
			tk.Reset(ispan)
			cspan = ispan
		}
	}
}

func (s *Slicer) upload(
	ctx context.Context,
	from, to primitive.Timestamp,
	compression compress.CompressionType,
	level *int,
) error {
	s.oplog.SetTailingSpan(from, to)
	fname := s.chunkPath(from, to, compression)
	// if use parent ctx, upload will be canceled on the "done" signal
	size, err := storage.Upload(ctx, s.oplog, s.storage, compression, level, fname, -1)
	if err != nil {
		// PITR chunks have no metadata to indicate any failed state and if something went
		// wrong during the data read we may end up with an already created file. Although
		// the failed range won't be saved in db as the available for restore. It would get
		// in there after the storage resync. see: https://jira.percona.com/browse/PBM-602
		s.l.Debug("remove %s due to upload errors", fname)
		derr := s.storage.Delete(fname)
		if derr != nil {
			s.l.Error("remove %s: %v", fname, derr)
		}
		return errors.Wrapf(err, "unable to upload chunk %v.%v", from, to)
	}

	meta := oplog.OplogChunk{
		RS:          s.rs,
		FName:       fname,
		Compression: compression,
		StartTS:     from,
		EndTS:       to,
		Size:        size,
	}
	err = oplog.PITRAddChunk(ctx, s.leadClient, meta)
	if err != nil {
		return errors.Wrapf(err, "unable to save chunk meta %v", meta)
	}

	return nil
}

func formatts(t primitive.Timestamp) string {
	return time.Unix(int64(t.T), 0).UTC().Format("2006-01-02T15:04:05")
}

func (s *Slicer) getOpLock(ctx context.Context, l *lock.LockHeader, t time.Duration) (lock.LockData, error) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()

	var lck lock.LockData
	for j := 0; j < int(t.Seconds()); j++ {
		var err error
		lck, err = lock.GetLockData(ctx, s.leadClient, l)
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return lck, errors.Wrap(err, "get")
		}
		if lck.Type != ctrl.CmdUndefined {
			return lck, nil
		}
		<-tk.C
	}

	return lck, nil
}

func (s *Slicer) backupStartTS(ctx context.Context, opid string, t time.Duration) (primitive.Timestamp, error) {
	var ts primitive.Timestamp
	tk := time.NewTicker(time.Second)
	defer tk.Stop()

	for j := 0; j < int(t.Seconds()); j++ {
		b, err := backup.GetBackupByOPID(ctx, s.leadClient, opid)
		if err != nil && !errors.Is(err, errors.ErrNotFound) {
			return ts, errors.Wrap(err, "get backup meta")
		}
		if b != nil && b.FirstWriteTS.T > 1 {
			return b.FirstWriteTS, nil
		}
		<-tk.C
	}

	return ts, errors.New("run out of tries")
}

// !!! should be agreed with pbm.PITRmetaFromFName()
func (s *Slicer) chunkPath(first, last primitive.Timestamp, c compress.CompressionType) string {
	return ChunkName(s.rs, first, last, c)
}

func ChunkName(rs string, first, last primitive.Timestamp, c compress.CompressionType) string {
	ft := time.Unix(int64(first.T), 0).UTC()
	lt := time.Unix(int64(last.T), 0).UTC()

	name := strings.Builder{}
	if len(defs.PITRfsPrefix) > 0 {
		name.WriteString(defs.PITRfsPrefix)
		name.WriteString("/")
	}
	name.WriteString(rs)
	name.WriteString("/")
	name.WriteString(ft.Format("20060102"))
	name.WriteString("/")
	name.WriteString(ft.Format("20060102150405"))
	name.WriteString("-")
	name.WriteString(strconv.Itoa(int(first.I)))
	name.WriteString(".")
	name.WriteString(lt.Format("20060102150405"))
	name.WriteString("-")
	name.WriteString(strconv.Itoa(int(last.I)))
	name.WriteString(".oplog")
	name.WriteString(c.Suffix())

	return name.String()
}