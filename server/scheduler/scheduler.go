package scheduler

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	pb "github.com/bleenco/abstruse/pb"
	"github.com/bleenco/abstruse/pkg/gitscm"
	"github.com/bleenco/abstruse/pkg/lib"
	"github.com/bleenco/abstruse/server/core"
	"github.com/bleenco/abstruse/server/ws"
	"github.com/drone/go-scm/scm"
	"go.uber.org/zap"
)

// New returns new scheduler.
func New(
	workers core.WorkerRegistry,
	jobStore core.JobStore,
	buildStore core.BuildStore,
	logger *zap.Logger,
	ws *ws.Server,
) core.Scheduler {
	s := &scheduler{
		ready:      make(chan struct{}, 1),
		interval:   time.Minute,
		workers:    workers,
		jobStore:   jobStore,
		buildStore: buildStore,
		logger:     logger.With(zap.String("type", "scheduler")).Sugar(),
		pending:    make(map[uint]*jobType),
		ws:         ws,
		ctx:        context.Background(),
	}
	go s.run()
	return s
}

type scheduler struct {
	mu         sync.Mutex
	ready      chan struct{}
	paused     bool
	interval   time.Duration
	workers    core.WorkerRegistry
	jobStore   core.JobStore
	buildStore core.BuildStore
	logger     *zap.SugaredLogger
	queued     []*core.Job
	pending    map[uint]*jobType
	ws         *ws.Server
	ctx        context.Context
}

type jobType struct {
	job *core.Job
	pb  *pb.Job
}

func (s *scheduler) Next(job *core.Job) error {
	s.logger.Infof("scheduling job %d from build %d...", job.ID, job.BuildID)
	s.Stop(job.ID)
	s.mu.Lock()
	s.queued = append(s.queued, job)
	s.mu.Unlock()

	job.Status = "queued"
	job.StartTime = nil
	job.EndTime = nil
	if err := s.saveJob(job); err != nil {
		s.logger.Errorf("error saving job %d: %v", job.ID, err.Error())
	}
	s.logger.Infof("job %d scheduled", job.ID)

	s.next(s.ctx)

	return nil
}

func (s *scheduler) Stop(id uint) (bool, error) {
	if job, err := s.findJob(id); err == nil {
		s.removeJob(id)
		job.Status = "failing"
		job.EndTime = lib.TimeNow()
		s.logger.Infof("job %d removed from queue", id)
		if err := s.saveJob(job); err == nil {
			return true, nil
		}
		s.logger.Errorf("error saving job %d: %v", job.ID, err.Error())
		return false, nil
	}

	if job, ok := s.pending[id]; ok {
		worker, err := s.getWorker(job.pb.WorkerId)
		if err != nil {
			job.job.Status = "failing"
			job.job.EndTime = lib.TimeNow()
			if err := s.saveJob(job.job); err != nil {
				s.logger.Errorf("error saving job %d: %v", job.job.ID, err.Error())
			}
			return false, err
		}

		stopped, err := worker.StopJob(job.pb)
		if err != nil {
			s.mu.Lock()
			delete(s.pending, job.job.ID)
			s.mu.Unlock()
		}

		s.logger.Infof("job %d stopped", id)
		job.job.Status = "failing"
		job.job.EndTime = lib.TimeNow()
		if err := s.saveJob(job.job); err != nil {
			s.logger.Errorf("error saving job %d: %v", job.job.ID, err.Error())
		}

		return stopped, nil
	}

	return false, nil
}

func (s *scheduler) RestartBuild(id uint) error {
	build, err := s.buildStore.Find(id)
	if err != nil {
		return err
	}
	build.StartTime = nil
	build.EndTime = nil
	if err := s.buildStore.Update(build); err != nil {
		s.logger.Errorf("error saving build %d: %v", build.ID, err.Error())
		return err
	}
	for _, job := range build.Jobs {
		job, _ := s.jobStore.Find(job.ID)
		s.Next(job)
	}

	return nil
}

func (s *scheduler) StopBuild(id uint) error {
	build, err := s.buildStore.Find(id)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(len(build.Jobs))
	for _, job := range build.Jobs {
		job, _ := s.jobStore.Find(job.ID)
		go func(job *core.Job) {
			s.Stop(job.ID)
			wg.Done()
		}(job)
	}
	wg.Wait()
	return s.updateBuildTime(id)
}

func (s *scheduler) Pause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	return nil
}

func (s *scheduler) Resume() error {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
	s.next(s.ctx)
	return nil
}

func (s *scheduler) JobLog(id uint) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if job, ok := s.pending[id]; ok {
		return strings.Join(job.pb.GetLog(), ""), nil
	}
	return "", fmt.Errorf("job not running")
}

func (s *scheduler) process() error {
	worker, err := s.findWorker()
	if err != nil {
		return err
	}
	if worker == nil {
		return nil
	}
	job, err := s.enqueueJob()
	if err != nil || job == nil {
		return nil
	}

	s.logger.Infof("processing job %d, sending to worker %s...", job.ID, worker.ID)
	go s.startJob(job, worker)

	return nil
}

func (s *scheduler) startJob(job *core.Job, worker *core.Worker) {
	worker.Lock()
	worker.Running++
	worker.Unlock()

	defer func() {
		worker.Lock()
		worker.Running--
		worker.Unlock()
	}()

	s.removeJob(job.ID)

	job.Status = "running"
	job.Log = ""
	job.StartTime = lib.TimeNow()
	job.EndTime = nil
	if err := s.saveJob(job); err != nil {
		s.logger.Errorf("error saving job %d: %v", job.ID, err.Error())
	}

	j := &pb.Job{
		Id:            uint64(job.ID),
		BuildId:       uint64(job.BuildID),
		Commands:      job.Commands,
		Image:         job.Image,
		Env:           job.Env,
		Url:           job.Build.Repository.URL,
		ProviderName:  job.Build.Repository.Provider.Name,
		ProviderURL:   job.Build.Repository.Provider.URL,
		ProviderToken: job.Build.Repository.Provider.AccessToken,
		Ref:           job.Build.Ref,
		CommitSHA:     job.Build.Commit,
		RepoName:      job.Build.Repository.FullName,
		Action:        pb.Job_JobStart,
		WorkerId:      worker.ID,
	}

	s.mu.Lock()
	s.pending[job.ID] = &jobType{job: job, pb: j}
	s.mu.Unlock()

	j, err := worker.StartJob(j)
	if err != nil {
		s.logger.Errorf("job %d errored: %v", job.ID, err.Error())
	}

	job.Status = j.GetStatus()
	job.Log = strings.Join(j.GetLog(), "")
	job.EndTime = lib.TimeNow()
	if err := s.saveJob(job); err != nil {
		s.logger.Errorf("error saving job %d: %v", job.ID, err.Error())
		log := strings.Join(j.GetLog(), "")
		job.Log = log[len(log)-65536:]
		if err := s.saveJob(job); err != nil {
			s.logger.Errorf("error saving job %d: %v", job.ID, err.Error())
		}
	}

	s.mu.Lock()
	delete(s.pending, job.ID)
	s.mu.Unlock()

	s.next(s.ctx)
}

func (s *scheduler) next(ctx context.Context) {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func (s *scheduler) enqueueJob() (*core.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.queued) > 0 {
		job := s.queued[0]
		s.queued = s.queued[1:]
		return job, nil
	}
	return nil, fmt.Errorf("no jobs queued")
}

func (s *scheduler) findJob(id uint) (*core.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, job := range s.queued {
		if job.ID == id {
			return job, nil
		}
	}

	return nil, fmt.Errorf("job not found")
}

func (s *scheduler) removeJob(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, job := range s.queued {
		if job.ID == id {
			s.queued = append(s.queued[:i], s.queued[i+1:]...)
			break
		}
	}
}

func (s *scheduler) findWorker() (*core.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	workers, err := s.workers.List()
	if err != nil {
		return nil, err
	}

	var worker *core.Worker
	var c int
	for _, w := range workers {
		w.Lock()
		diff := w.Max - w.Running
		if diff > c {
			worker, c = w, diff
		}
		w.Unlock()
	}

	return worker, nil
}

func (s *scheduler) getWorker(id string) (*core.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	workers, err := s.workers.List()
	if err != nil {
		return nil, err
	}

	for _, w := range workers {
		if w.ID == id {
			return w, nil
		}
	}

	return nil, fmt.Errorf("worker not found")
}

func (s *scheduler) saveJob(job *core.Job) error {
	if err := s.jobStore.Update(job); err != nil {
		s.logger.Errorf("error saving job %d: %v", job.ID, err.Error())
	}
	go s.broadcastJobStatus(job)
	return s.updateBuildTime(job.BuildID)
}

func (s *scheduler) broadcastJobStatus(job *core.Job) {
	sub := path.Clean(path.Join("/subs", "jobs"))
	event := map[string]interface{}{
		"buildID": job.BuildID,
		"jobID":   job.ID,
		"status":  job.Status,
	}
	if job.StartTime != nil {
		event["startTime"] = job.StartTime
	}
	if job.EndTime != nil {
		event["endTime"] = job.EndTime
	}
	s.ws.App.Broadcast(sub, event)
}

func (s *scheduler) updateBuildTime(id uint) error {
	build, err := s.buildStore.Find(id)
	if err != nil {
		return err
	}
	if build.StartTime != nil && build.EndTime != nil {
		return nil
	}

	alldone := true
	var startTime *time.Time
	var endTime *time.Time
	for _, j := range build.Jobs {
		if j.EndTime == nil {
			alldone = false
			break
		} else {
			if endTime == nil || j.EndTime.After(*endTime) {
				endTime = j.EndTime
			}
		}
		if startTime == nil || (j.StartTime != nil && j.StartTime.Before(*startTime)) {
			startTime = j.StartTime
		}
	}
	if startTime != nil {
		build.StartTime = startTime
		if err := s.buildStore.Update(build); err != nil {
			s.logger.Errorf("error saving build %d: %v", build.ID, err.Error())
			return err
		}
	}

	if alldone && endTime != nil {
		build.EndTime = endTime
		if err := s.buildStore.Update(build); err != nil {
			s.logger.Errorf("error saving build %d: %v", build.ID, err.Error())
			return err
		}

		success := true
		for _, j := range build.Jobs {
			if j.Status != "passing" {
				success = false
				break
			}
		}
		var status scm.State
		if success {
			status = scm.StateSuccess
		} else {
			status = scm.StateError
		}
		if err := s.sendStatus(build, status); err != nil {
			return err
		}
	}

	return nil
}

func (s *scheduler) sendStatus(build *core.Build, status scm.State) error {
	scm, err := gitscm.New(
		context.Background(),
		build.Repository.Provider.Name,
		build.Repository.Provider.URL,
		build.Repository.Provider.AccessToken,
	)
	if err != nil {
		return err
	}

	if err := scm.CreateStatus(
		build.Repository.FullName,
		build.Commit,
		fmt.Sprintf("%s/builds/%d", build.Repository.Provider.Host, build.ID),
		status,
	); err != nil {
		s.logger.Errorf("error sending build status to scm provider: %v", err.Error())
		return err
	}
	s.logger.Debugf("successfully sent build status to scm provider for repo %s", build.Repository.FullName)

	return nil
}

func (s *scheduler) run() error {
	s.logger.Infof("starting scheduler loop")
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-s.ready:
			s.process()
		case <-time.After(s.interval):
			s.next(s.ctx)
		}
	}
}
