package panel

import (
	"encoding/json"
	"log"
	"sort"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// 表：task_jobs（每账号一条最新后台作业，uid 为主键）。
// 「每账号一条」沿用旧 task-jobs.json 的语义：同一账号只留最近一次作业。
func (TaskJob) TableName() string { return "task_jobs" }

// legacyTaskJobsFile 旧的 JSON 存储文件名（与 state 文件同目录）。
const legacyTaskJobsFile = "task-jobs.json"

func init() {
	store.RegisterModels(&TaskJob{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "taskjobs",
		Name: legacyTaskJobsFile,
		Fn:   importLegacyTaskJobs,
	})
}

// importLegacyTaskJobs 把旧的 task-jobs.json 一次性导入数据库。
func importLegacyTaskJobs(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, legacyTaskJobsFile)
	if !ok {
		return nil
	}
	var f taskJobFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	latest := make(map[string]TaskJob, len(f.Jobs))
	for _, job := range f.Jobs {
		if job.UID == "" || job.ID == "" {
			continue
		}
		if len(job.Results) > maxTaskJobResults {
			job.Results = job.Results[len(job.Results)-maxTaskJobResults:]
		}
		if old, exists := latest[job.UID]; !exists || job.UpdatedAt.After(old.UpdatedAt) {
			latest[job.UID] = job
		}
	}
	jobs := make([]TaskJob, 0, len(latest))
	for _, job := range latest {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].UpdatedAt.After(jobs[j].UpdatedAt) })
	if len(jobs) > maxTaskJobs {
		jobs = jobs[:maxTaskJobs]
	}
	if err := store.ReplaceAll(s, jobs); err != nil {
		return err
	}
	log.Printf("[panel] 已从旧文件 %s 导入 %d 条后台任务记录", legacyTaskJobsFile, len(jobs))
	return nil
}
