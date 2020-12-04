package core

type (
	// Scheduler represents build jobs scheduler.
	Scheduler interface {
		// Next schedules job for execution.
		Next(*Job) error

		// Stop cancels scheduled or running job and returns
		// true if job has been stopped.
		Stop(uint) (bool, error)

		// RestartBuild restart the build or associated jobs.
		RestartBuild(uint) error

		// StopBuild stops the build or associated jobs.
		StopBuild(uint) error

		// Pause pauses the scheduler.
		Pause() error

		// Resume starts paused scheduler.
		Resume() error

		// JobLog returns jobs current log output.
		JobLog(uint) (string, error)
	}
)
