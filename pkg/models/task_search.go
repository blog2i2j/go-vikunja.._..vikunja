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
	"context"
	"strconv"
	"strings"
	"time"

	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/web"
	"github.com/typesense/typesense-go/typesense/api"
	"github.com/typesense/typesense-go/typesense/api/pointer"

	"xorm.io/builder"
	"xorm.io/xorm"
	"xorm.io/xorm/schemas"
)

type taskSearcher interface {
	Search(opts *taskSearchOptions) (tasks []*Task, totalCount int64, err error)
}

type dbTaskSearcher struct {
	s                   *xorm.Session
	a                   web.Auth
	hasFavoritesProject bool
}

func getOrderByDBStatement(opts *taskSearchOptions) (orderby string, err error) {
	// Since xorm does not use placeholders for order by, it is possible to expose this with sql injection if we're directly
	// passing user input to the db.
	// As a workaround to prevent this, we check for valid column names here prior to passing it to the db.
	for i, param := range opts.sortby {
		// Validate the params
		if err := param.validate(); err != nil {
			return "", err
		}

		var prefix string
		if param.sortBy == taskPropertyPosition {
			prefix = "task_positions."
		}

		if param.sortBy == taskPropertyID {
			prefix = "tasks."
		}

		// Mysql sorts columns with null values before ones without null value.
		// Because it does not have support for NULLS FIRST or NULLS LAST we work around this by
		// first sorting for null (or not null) values and then the order we actually want to.
		if db.Type() == schemas.MYSQL {
			orderby += prefix + "`" + param.sortBy + "` IS NULL, "
		}

		orderby += prefix + "`" + param.sortBy + "` " + param.orderBy.String()

		// Postgres and sqlite allow us to control how columns with null values are sorted.
		// To make that consistent with the sort order we have and other dbms, we're adding a separate clause here.
		if db.Type() == schemas.POSTGRES || db.Type() == schemas.SQLITE {
			orderby += " NULLS LAST"
		}

		if (i + 1) < len(opts.sortby) {
			orderby += ", "
		}
	}

	return
}

func convertFiltersToDBFilterCond(rawFilters []*taskFilter, includeNulls bool) (filterCond builder.Cond, err error) {

	var dbFilters = make([]builder.Cond, 0, len(rawFilters))
	// To still find tasks with nil values, we exclude 0s when comparing with >/< values.
	for _, f := range rawFilters {

		if nested, is := f.value.([]*taskFilter); is {
			nestedDBFilters, err := convertFiltersToDBFilterCond(nested, includeNulls)
			if err != nil {
				return nil, err
			}
			dbFilters = append(dbFilters, nestedDBFilters)
			continue
		}

		if f.field == "reminders" {
			filter, err := getFilterCond(&taskFilter{
				// recreating the struct here to avoid modifying it when reusing the opts struct
				field:      "reminder",
				value:      f.value,
				comparator: f.comparator,
				isNumeric:  f.isNumeric,
			}, includeNulls)
			if err != nil {
				return nil, err
			}
			dbFilters = append(dbFilters, getFilterCondForSeparateTable("task_reminders", filter))
			continue
		}

		if f.field == "assignees" {
			if f.comparator == taskFilterComparatorLike {
				return
			}
			filter, err := getFilterCond(&taskFilter{
				// recreating the struct here to avoid modifying it when reusing the opts struct
				field:      "username",
				value:      f.value,
				comparator: f.comparator,
				isNumeric:  f.isNumeric,
			}, includeNulls)
			if err != nil {
				return nil, err
			}

			assigneeFilter := builder.In("user_id",
				builder.Select("id").
					From("users").
					Where(filter),
			)
			dbFilters = append(dbFilters, getFilterCondForSeparateTable("task_assignees", assigneeFilter))
			continue
		}

		if f.field == "labels" || f.field == "label_id" {
			filter, err := getFilterCond(&taskFilter{
				// recreating the struct here to avoid modifying it when reusing the opts struct
				field:      "label_id",
				value:      f.value,
				comparator: f.comparator,
				isNumeric:  f.isNumeric,
			}, includeNulls)
			if err != nil {
				return nil, err
			}

			dbFilters = append(dbFilters, getFilterCondForSeparateTable("label_tasks", filter))
			continue
		}

		if f.field == "parent_project" || f.field == "parent_project_id" {
			filter, err := getFilterCond(&taskFilter{
				// recreating the struct here to avoid modifying it when reusing the opts struct
				field:      "parent_project_id",
				value:      f.value,
				comparator: f.comparator,
				isNumeric:  f.isNumeric,
			}, includeNulls)
			if err != nil {
				return nil, err
			}

			cond := builder.In(
				"project_id",
				builder.
					Select("id").
					From("projects").
					Where(filter),
			)
			dbFilters = append(dbFilters, cond)
			continue
		}

		filter, err := getFilterCond(f, includeNulls)
		if err != nil {
			return nil, err
		}
		dbFilters = append(dbFilters, filter)
	}

	if len(dbFilters) > 0 {
		if len(dbFilters) == 1 {
			filterCond = dbFilters[0]
		} else {
			for i, f := range dbFilters {
				if len(dbFilters) > i+1 {
					switch rawFilters[i+1].join {
					case filterConcatOr:
						filterCond = builder.Or(filterCond, f, dbFilters[i+1])
					case filterConcatAnd:
						filterCond = builder.And(filterCond, f, dbFilters[i+1])
					}
				}
			}
		}
	}

	return filterCond, nil
}

//nolint:gocyclo
func (d *dbTaskSearcher) Search(opts *taskSearchOptions) (tasks []*Task, totalCount int64, err error) {

	orderby, err := getOrderByDBStatement(opts)
	if err != nil {
		return nil, 0, err
	}

	var joinTaskBuckets bool
	for _, filter := range opts.parsedFilters {
		if filter.field == taskPropertyBucketID {
			joinTaskBuckets = true
			break
		}
	}

	filterCond, err := convertFiltersToDBFilterCond(opts.parsedFilters, opts.filterIncludeNulls)
	if err != nil {
		return nil, 0, err
	}

	// Then return all tasks for that projects
	var where builder.Cond

	if opts.search != "" {
		where =
			builder.Or(
				db.ILIKE("title", opts.search),
				db.ILIKE("description", opts.search),
			)

		searchIndex := getTaskIndexFromSearchString(opts.search)
		if searchIndex > 0 {
			where = builder.Or(where, builder.Eq{"`index`": searchIndex})
		}
	}

	var projectIDCond builder.Cond
	var favoritesCond builder.Cond
	if len(opts.projectIDs) > 0 {
		projectIDCond = builder.In("project_id", opts.projectIDs)
	}

	if d.hasFavoritesProject {
		// All favorite tasks for that user
		favCond := builder.
			Select("entity_id").
			From("favorites").
			Where(
				builder.And(
					builder.Eq{"user_id": d.a.GetID()},
					builder.Eq{"kind": FavoriteKindTask},
				))

		favoritesCond = builder.In("tasks.id", favCond)
	}

	limit, start := getLimitFromPageIndex(opts.page, opts.perPage)
	cond := builder.And(builder.Or(projectIDCond, favoritesCond), where, filterCond)

	var distinct = "tasks.*"
	if strings.Contains(orderby, "task_positions.") {
		distinct += ", task_positions.position"
	}

	if opts.expand == TaskCollectionExpandSubtasks {
		cond = builder.And(cond, builder.IsNull{"task_relations.id"})
	}

	query := d.s.
		Distinct(distinct).
		Where(cond)
	if limit > 0 {
		query = query.Limit(limit, start)
	}

	for _, param := range opts.sortby {
		if param.sortBy == taskPropertyPosition {
			query = query.Join("LEFT", "task_positions", "task_positions.task_id = tasks.id AND task_positions.project_view_id = ?", param.projectViewID)
			break
		}
	}

	if joinTaskBuckets {
		query = query.Join("LEFT", "task_buckets", "task_buckets.task_id = tasks.id")
	}
	if opts.expand == TaskCollectionExpandSubtasks {
		query = query.Join("LEFT", "task_relations", "tasks.id = task_relations.task_id and task_relations.relation_kind = 'parenttask'")
	}

	tasks = []*Task{}
	err = query.
		OrderBy(orderby).
		Find(&tasks)
	if err != nil {
		return nil, totalCount, err
	}

	// fetch subtasks when expanding
	if opts.expand == TaskCollectionExpandSubtasks {
		subtasks := []*Task{}

		taskIDs := []int64{}
		for _, task := range tasks {
			taskIDs = append(taskIDs, task.ID)
		}

		inQuery := builder.Dialect(db.GetDialect()).
			Select("*").
			From("task_relations").
			Where(builder.In("task_id", taskIDs))
		inSQL, inArgs, err := inQuery.ToSQL()
		if err != nil {
			return nil, totalCount, err
		}

		inSQL = strings.TrimPrefix(inSQL, "SELECT * FROM task_relations WHERE")

		err = d.s.SQL(`SELECT * FROM tasks WHERE id IN (WITH RECURSIVE sub_tasks AS (
		SELECT task_id,
			other_task_id,
			relation_kind,
			created_by_id,
			created
		FROM task_relations
		WHERE `+inSQL+`
		AND relation_kind = '`+string(RelationKindSubtask)+`'

		UNION ALL

		SELECT tr.task_id,
			tr.other_task_id,
			tr.relation_kind,
			tr.created_by_id,
			tr.created
		FROM task_relations tr
		INNER JOIN
		sub_tasks st ON tr.task_id = st.other_task_id
		WHERE tr.relation_kind = '`+string(RelationKindSubtask)+`')
		SELECT other_task_id
		FROM sub_tasks)`, inArgs...).Find(&subtasks)
		if err != nil {
			return nil, totalCount, err
		}

		tasks = append(tasks, subtasks...)
	}

	queryCount := d.s.Where(cond)
	if joinTaskBuckets {
		queryCount = queryCount.Join("LEFT", "task_buckets", "task_buckets.task_id = tasks.id")
	}
	if opts.expand == TaskCollectionExpandSubtasks {
		queryCount = queryCount.Join("LEFT", "task_relations", "tasks.id = task_relations.task_id and task_relations.relation_kind = 'parenttask'")
	}
	totalCount, err = queryCount.
		Select("count(DISTINCT tasks.id)").
		Count(&Task{})
	return
}

type typesenseTaskSearcher struct {
	s *xorm.Session
}

func convertFilterValues(value interface{}) string {
	if _, is := value.([]interface{}); is {
		filter := []string{}
		for _, v := range value.([]interface{}) {
			filter = append(filter, convertFilterValues(v))
		}

		return strings.Join(filter, ",")
	}

	if stringSlice, is := value.([]string); is {
		filter := []string{}
		for _, v := range stringSlice {
			filter = append(filter, convertFilterValues(v))
		}

		return strings.Join(filter, ",")
	}

	switch v := value.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		if v {
			return "true"
		}

		return "false"
	case time.Time:
		return strconv.FormatInt(v.Unix(), 10)
	default:
		log.Errorf("Unknown search type for value %v of type %T", value, value)
	}

	return ""
}

// Parsing and rebuilding the filter for Typesense has the advantage that we have more control over
// what Typesense finally gets to see.
func convertParsedFilterToTypesense(rawFilters []*taskFilter) (filterBy string, err error) {

	filters := []string{}

	for _, f := range rawFilters {

		if nested, is := f.value.([]*taskFilter); is {
			nestedDBFilters, err := convertParsedFilterToTypesense(nested)
			if err != nil {
				return "", err
			}
			filters = append(filters, "("+nestedDBFilters+")")
			continue
		}

		if f.field == "reminders" {
			f.field = "reminders.reminder"
		}

		if f.field == "assignees" {
			f.field = "assignees.username"
		}

		if f.field == "labels" || f.field == "label_id" {
			f.field = "labels.id"
		}

		if f.field == "project" {
			f.field = "project_id"
		}

		if f.field == "bucket_id" {
			f.field = "buckets"
		}

		filter := f.field

		switch f.comparator {
		case taskFilterComparatorEquals:
			filter += ":="
		case taskFilterComparatorNotEquals:
			filter += ":!="
		case taskFilterComparatorGreater:
			filter += ":>"
		case taskFilterComparatorGreateEquals:
			filter += ":>="
		case taskFilterComparatorLess:
			filter += ":<"
		case taskFilterComparatorLessEquals:
			filter += ":<="
		case taskFilterComparatorLike:
			filter += ":"
		case taskFilterComparatorIn:
			filter += ":["
		case taskFilterComparatorInvalid:
		// Nothing to do
		default:
			filter += ":="
		}

		filter += convertFilterValues(f.value)

		if f.comparator == taskFilterComparatorIn {
			filter += "]"
		}

		filters = append(filters, filter)
	}

	if len(filters) > 0 {
		if len(filters) == 1 {
			filterBy = filters[0]
		} else {
			for i, f := range filters {
				if len(filters) > i+1 {
					switch rawFilters[i+1].join {
					case filterConcatOr:
						filterBy = f + " || " + filters[i+1]
					case filterConcatAnd:
						filterBy = f + " && " + filters[i+1]
					}
				}
			}
		}
	}

	return
}

func (t *typesenseTaskSearcher) Search(opts *taskSearchOptions) (tasks []*Task, totalCount int64, err error) {

	projectIDStrings := []string{}
	for _, id := range opts.projectIDs {
		projectIDStrings = append(projectIDStrings, strconv.FormatInt(id, 10))
	}

	filter, err := convertParsedFilterToTypesense(opts.parsedFilters)
	if err != nil {
		return nil, 0, err
	}

	filterBy := []string{"project_id: [" + strings.Join(projectIDStrings, ", ") + "]"}

	if filter != "" {
		filterBy = append(filterBy, "("+filter+")")
	}

	var sortbyFields []string
	var usedParams int
	for _, param := range opts.sortby {

		if opts.isSavedFilter && param.sortBy == taskPropertyPosition {
			continue
		}

		// Validate the params
		if err := param.validate(); err != nil {
			return nil, totalCount, err
		}

		sortBy := param.sortBy

		// Typesense does not allow sorting by ID, so we sort by created timestamp instead
		if param.sortBy == taskPropertyID {
			sortBy = taskPropertyCreated
		}

		if param.sortBy == taskPropertyPosition {
			sortBy = "positions.view_" + strconv.FormatInt(param.projectViewID, 10)
		}

		sortbyFields = append(sortbyFields, sortBy+"(missing_values:last):"+param.orderBy.String())

		if usedParams == 2 {
			// Typesense supports up to 3 sorting parameters
			// https://typesense.org/docs/0.25.0/api/search.html#ranking-and-sorting-parameters
			break
		}

		usedParams++
	}

	sortby := strings.Join(sortbyFields, ",")

	////////////////
	// Actual search

	if opts.search == "" {
		opts.search = "*"
	}

	params := &api.SearchCollectionParams{
		Q:                opts.search,
		QueryBy:          "title, identifier, description, comments.comment",
		Page:             pointer.Int(opts.page),
		ExhaustiveSearch: pointer.True(),
		FilterBy:         pointer.String(strings.Join(filterBy, " && ")),
	}

	if opts.perPage > 0 {
		if opts.perPage > 250 {
			log.Warningf("Typesense only supports up to 250 results per page, requested %d.", opts.perPage)
			opts.perPage = 250
		}
		params.PerPage = pointer.Int(opts.perPage)
	}

	if sortby != "" {
		params.SortBy = pointer.String(sortby)
	}

	result, err := typesenseClient.Collection("tasks").
		Documents().
		Search(context.Background(), params)
	if err != nil {
		return
	}

	taskIDs := []int64{}
	for _, h := range *result.Hits {
		hit := *h.Document
		taskID, err := strconv.ParseInt(hit["id"].(string), 10, 64)
		if err != nil {
			return nil, 0, err
		}
		taskIDs = append(taskIDs, taskID)
	}

	tasks = []*Task{}

	orderby, err := getOrderByDBStatement(opts)
	if err != nil {
		return nil, 0, err
	}

	var distinct = "tasks.*"
	if strings.Contains(orderby, "task_positions.") {
		distinct += ", task_positions.position"
	}

	query := t.s.
		Distinct(distinct).
		In("id", taskIDs).
		OrderBy(orderby)

	for _, param := range opts.sortby {
		if param.sortBy == taskPropertyPosition {
			query = query.Join("LEFT", "task_positions", "task_positions.task_id = tasks.id AND task_positions.project_view_id = ?", param.projectViewID)
			break
		}
	}

	err = query.Find(&tasks)
	return tasks, int64(*result.Found), err
}
