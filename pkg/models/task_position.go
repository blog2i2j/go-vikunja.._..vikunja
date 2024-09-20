// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public Licensee as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public Licensee for more details.
//
// You should have received a copy of the GNU Affero General Public Licensee
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package models

import (
	"math"

	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/web"
	"xorm.io/xorm"
)

type TaskPosition struct {
	// The ID of the task this position is for
	TaskID int64 `xorm:"bigint not null index" json:"task_id" param:"task"`
	// The project view this task is related to
	ProjectViewID int64 `xorm:"bigint not null index" json:"project_view_id"`
	// The position of the task - any task project can be sorted as usual by this parameter.
	// When accessing tasks via kanban buckets, this is primarily used to sort them based on a range
	// We're using a float64 here to make it possible to put any task within any two other tasks (by changing the number).
	// You would calculate the new position between two tasks with something like task3.position = (task2.position - task1.position) / 2.
	// A 64-Bit float leaves plenty of room to initially give tasks a position with 2^16 difference to the previous task
	// which also leaves a lot of room for rearranging and sorting later.
	// Positions are always saved per view. They will automatically be set if you request the tasks through a view
	// endpoint, otherwise they will always be 0. To update them, take a look at the Task Position endpoint.
	Position float64 `xorm:"double not null" json:"position"`

	web.CRUDable `xorm:"-" json:"-"`
	web.Rights   `xorm:"-" json:"-"`
}

func (tp *TaskPosition) TableName() string {
	return "task_positions"
}

func (tp *TaskPosition) CanUpdate(s *xorm.Session, a web.Auth) (bool, error) {
	t := &Task{ID: tp.TaskID}
	return t.CanUpdate(s, a)
}

// Update is the handler to update a task position
// @Summary Updates a task position
// @Description Updates a task position.
// @tags task
// @Accept json
// @Produce json
// @Security JWTKeyAuth
// @Param id path int true "Task ID"
// @Param view body models.TaskPosition true "The task position with updated values you want to change."
// @Success 200 {object} models.TaskPosition "The updated task position."
// @Failure 400 {object} web.HTTPError "Invalid task position object provided."
// @Failure 500 {object} models.Message "Internal error"
// @Router /tasks/{id}/position [post]
func (tp *TaskPosition) Update(s *xorm.Session, a web.Auth) (err error) {

	// Update all positions if the newly saved position is < 0.1
	var shouldRecalculate bool
	var view *ProjectView
	if tp.Position < 0.1 {
		shouldRecalculate = true
		view, err = GetProjectViewByID(s, tp.ProjectViewID)
		if err != nil {
			return err
		}
	}

	exists, err := s.
		Where("task_id = ? AND project_view_id = ?", tp.TaskID, tp.ProjectViewID).
		Exist(&TaskPosition{})
	if err != nil {
		return err
	}

	if !exists {
		_, err = s.Insert(tp)
		if err != nil {
			return
		}
		if shouldRecalculate {
			return RecalculateTaskPositions(s, view, a)
		}
		return nil
	}

	_, err = s.
		Where("task_id = ?", tp.TaskID).
		Cols("project_view_id", "position").
		Update(tp)
	if err != nil {
		return
	}

	if shouldRecalculate {
		return RecalculateTaskPositions(s, view, a)
	}

	return triggerTaskUpdatedEventForTaskID(s, a, tp.TaskID)
}

func RecalculateTaskPositions(s *xorm.Session, view *ProjectView, a web.Auth) (err error) {

	// Using the collection so that we get all tasks, even in cases where we're dealing with a saved filter underneath
	tc := &TaskCollection{
		ProjectID: view.ProjectID,
	}
	if view.ProjectID < -1 {
		tc.ProjectID = 0
	}

	projects, err := getRelevantProjectsFromCollection(s, a, tc)
	if err != nil {
		return err
	}

	opts := &taskSearchOptions{
		sortby: []*sortParam{
			{
				projectViewID: view.ID,
				sortBy:        taskPropertyPosition,
				orderBy:       orderAscending,
			},
		},
	}

	allTasks, _, _, err := getRawTasksForProjects(s, projects, a, opts)
	if err != nil {
		return
	}
	if len(allTasks) == 0 {
		return
	}

	maxPosition := math.Pow(2, 32)
	newPositions := make([]*TaskPosition, 0, len(allTasks))

	for i, task := range allTasks {

		currentPosition := maxPosition / float64(len(allTasks)) * (float64(i + 1))

		newPositions = append(newPositions, &TaskPosition{
			TaskID:        task.ID,
			ProjectViewID: view.ID,
			Position:      currentPosition,
		})
	}

	_, err = s.
		Where("project_view_id = ?", view.ID).
		Delete(&TaskPosition{})
	if err != nil {
		return
	}

	_, err = s.Insert(newPositions)
	if err != nil {
		return
	}
	return events.Dispatch(&TaskPositionsRecalculatedEvent{
		NewTaskPositions: newPositions,
	})
}

func getPositionsForView(s *xorm.Session, view *ProjectView) (positions []*TaskPosition, err error) {
	positions = []*TaskPosition{}
	err = s.
		Where("project_view_id = ?", view.ID).
		Find(&positions)
	return
}
