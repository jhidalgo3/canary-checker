package cache

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/flanksource/canary-checker/pkg"
	"github.com/flanksource/commons/duration"
	"github.com/flanksource/commons/logger"
	"github.com/jackc/pgx/v4"
)

type Querier interface {
	Query(ctx context.Context, query string, args ...interface{}) (pgx.Rows, error)
}

func parseDuration(d string, name string) (clause string, arg interface{}, err error) {
	if d == "" {
		return "", nil, nil
	}
	dur, err := duration.ParseDuration(d)
	if err == nil {
		return fmt.Sprintf("(NOW() at TIME ZONE 'utc' - Interval '1 minute' * :%s)", name), dur.Minutes(), nil
	}
	if timestamp, err := time.Parse(time.RFC3339, d); err == nil {
		return ":" + name, timestamp, nil
	}
	return "", nil, fmt.Errorf("start time must be a duration or RFC3339 timestamp")
}

func (q QueryParams) GetWhereClause() (string, map[string]interface{}, error) {
	clause := ""
	args := make(map[string]interface{})
	and := " AND "
	if q.Check != "" {
		clause = "check_key = :check_key"
		args["check_key"] = q.Check
	}
	if q.Start != "" && q.End == "" {
		if clause != "" {
			clause += and
		}
		start, arg, err := parseDuration(q.Start, "start")
		if err != nil {
			return "", nil, err
		}
		args["start"] = arg
		clause += "time > " + start
	} else if q.Start == "" && q.End != "" {
		if clause != "" {
			clause += and
		}
		end, arg, err := parseDuration(q.End, "end")
		if err != nil {
			return "", nil, err
		}
		args["end"] = arg
		clause += "time < " + end
	}
	if q.Start != "" && q.End != "" {
		if clause != "" {
			clause += and
		}
		start, arg, err := parseDuration(q.Start, "start")
		if err != nil {
			return "", nil, err
		}
		args["start"] = arg
		end, arg, err := parseDuration(q.End, "end")
		if err != nil {
			return "", nil, err
		}
		args["end"] = arg
		clause += "time BETWEEN " + start + and + end
	}
	return strings.TrimSpace(clause), args, nil
}

func (q QueryParams) ExecuteDetails(db Querier) ([]pkg.Timeseries, error) {
	clause, namedArgs, err := q.GetWhereClause()
	if err != nil {
		return nil, err
	}
	namedArgs["limit"] = q.StatusCount
	keyIndex := 3
	messageIndex := 4
	errorIndex := 5

	sql := "SELECT time,duration,status "
	if q.Check == "" {
		sql += ", check_key"
	}
	if q.IncludeMessages {
		sql += ", message, error"
		if q.Check != "" {
			messageIndex -= 1
			errorIndex -= 1
		}
	}
	sql += fmt.Sprintf(`
	FROM check_statuses
	WHERE %s
	LIMIT :limit
`, clause)
	rows, err := exec(db, q, sql, namedArgs)
	if err != nil {
		return nil, err
	}

	var results []pkg.Timeseries
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		result := pkg.Timeseries{
			Time:     vals[0].(time.Time).Format(time.RFC3339),
			Duration: intV(vals[1]),
			Status:   vals[2].(bool),
		}
		if q.Check == "" {
			result.Key = vals[keyIndex].(string)
		}
		if q.IncludeMessages {
			result.Message = vals[messageIndex].(string)
			result.Error = vals[errorIndex].(string)
		}
		results = append(results, result)
	}
	return results, nil
}

func exec(db Querier, q QueryParams, sql string, namedArgs map[string]interface{}) (pgx.Rows, error) {
	if q.Trace {
		sqlDebug := convertNamedParamsDebug(sql, namedArgs)
		logger.Tracef(sqlDebug)
	}

	positionalSQL, args := convertNamedParams(sql, namedArgs)

	rows, err := db.Query(context.Background(), positionalSQL, args...)

	if err != nil {
		logger.Debugf("Error executing query: %v\n%s\n args=%v", err, positionalSQL, args)
	}
	return rows, err
}

func (q QueryParams) ExecuteSummary(db Querier) (pkg.Checks, error) {
	clause, namedArgs, err := q.GetWhereClause()
	if err != nil {
		return nil, err
	}

	statusColumns := ""
	if q.IncludeMessages {
		statusColumns += ", 'message', message, 'error', error"
	}
	sql := fmt.Sprintf(`
SELECT checks.key,
  passed.passed,
  failed.failed,
  stats.p99, stats.p97, stats.p95,
	statii,
	canary_name as canaryName,
	check_type as type,
	description,
	display_type as displayType,
	endpoint,
	icon,
	id,
	interval,
	labels,
	name,
	namespace,
	owner,
	runner_labels as runnerLabels,
	runner_name as runnerName,
	schedule,
	severity
	FROM checks checks
  FULL JOIN (
  	SELECT check_key,
			percentile_disc(0.99) within group (order by check_statuses.duration) as p99,
			percentile_disc(0.97) within group (order by check_statuses.duration) as p97,
			percentile_disc(0.05) within group (order by check_statuses.duration) as p95
			FROM check_statuses WHERE %s  GROUP BY check_key
  ) as stats ON stats.check_key = checks.key
  FULL JOIN (
    SELECT check_key,
      count(*) as failed
		FROM check_statuses
    WHERE status = false  AND %s
    GROUP BY check_key
  ) as failed ON failed.check_key = checks.key
  FULL JOIN (
    SELECT check_key,
      count(*) as passed
		FROM check_statuses
    WHERE  status = true AND %s
    GROUP BY check_key
  ) as passed ON passed.check_key = checks.key
		FULL JOIN (
			SELECT check_key, json_agg(json_build_object('status',status,'duration',duration,'time',time %s)) as statii
	FROM (
			SELECT check_key,
				status,
				time,
				duration,
				message,
				error,
				rank() OVER (
					PARTITION BY check_key
					ORDER BY time DESC
				)
			FROM check_statuses
			WHERE  %s
		) check_statuses
	WHERE rank <= :count
	GROUP by check_key
		) as statuses ON statuses.check_key = checks.key
		WHERE passed.passed > 0 OR failed.failed > 0
	`, clause, clause, clause, statusColumns, clause)

	if q.StatusCount == 0 {
		q.StatusCount = 5
	}
	namedArgs["count"] = q.StatusCount

	rows, err := exec(db, q, sql, namedArgs)
	if err != nil {
		return nil, err
	}

	checks := pkg.Checks{}
	for rows.Next() {
		var check = pkg.Check{}
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		check.Key = vals[0].(string)
		check.Uptime.Passed = intV(vals[1])
		check.Uptime.Failed = intV(vals[2])
		check.Latency.Percentile99 = float64V(vals[3])
		check.Latency.Percentile97 = float64V(vals[4])
		check.Latency.Percentile95 = float64V(vals[5])
		check.CanaryName = vals[7].(string)
		check.Type = vals[8].(string)
		check.Description = vals[9].(string)
		check.DisplayType = vals[10].(string)
		check.Endpoint = vals[11].(string)
		check.Icon = vals[12].(string)
		check.ID = vals[13].(string)
		check.Interval = uint64(intV(vals[14]))
		check.Labels = mapStringString(vals[15])
		check.Name = vals[16].(string)
		check.Namespace = vals[17].(string)
		check.Owner = vals[18].(string)
		check.RunnerLabels = mapStringString(vals[19])
		check.RunnerName = vals[20].(string)
		check.Schedule = vals[21].(string)
		check.Severity = vals[22].(string)

		if vals[6] != nil {
			for _, status := range vals[6].([]interface{}) {
				s := status.(map[string]interface{})
				check.Statuses = append(check.Statuses, pkg.CheckStatus{
					Status:   s["status"].(bool),
					Time:     s["time"].(string),
					Duration: intV(s["duration"]),
					Message:  stringV(s["message"]),
					Error:    stringV(s["error"]),
				})
			}
		}
		if q.Trace {
			logger.Infof("%s", check.String())
		}
		checks = append(checks, &check)
	}
	return checks, err
}
