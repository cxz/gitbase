package analyzer

import (
	"strings"

	errors "gopkg.in/src-d/go-errors.v1"
	"gopkg.in/src-d/go-mysql-server.v0/sql"
	"gopkg.in/src-d/go-mysql-server.v0/sql/expression"
	"gopkg.in/src-d/go-mysql-server.v0/sql/plan"
)

// DefaultRules to apply when analyzing nodes.
var DefaultRules = []Rule{
	{"resolve_subqueries", resolveSubqueries},
	{"resolve_tables", resolveTables},
	{"qualify_columns", qualifyColumns},
	{"resolve_columns", resolveColumns},
	{"resolve_database", resolveDatabase},
	{"resolve_star", resolveStar},
	{"resolve_functions", resolveFunctions},
	{"pushdown", pushdown},
	{"optimize_distinct", optimizeDistinct},
}

var (
	// ErrColumnTableNotFound is returned when the column does not exist in a
	// the table.
	ErrColumnTableNotFound = errors.NewKind("table %q does not have column %q")
	// ErrAmbiguousColumnName is returned when there is a column reference that
	// is present in more than one table.
	ErrAmbiguousColumnName = errors.NewKind("ambiguous column name %q, it's present in all these tables: %v")
	// ErrFieldMissing is returned when the field is not on the schema.
	ErrFieldMissing = errors.NewKind("field %q is not on schema")
)

func resolveSubqueries(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("resolving subqueries")
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		switch n := n.(type) {
		case *plan.SubqueryAlias:
			a.Log("found subquery %q with child of type %T", n.Name(), n.Child)
			child, err := a.Analyze(n.Child)
			if err != nil {
				return nil, err
			}
			return plan.NewSubqueryAlias(n.Name(), child), nil
		default:
			return n, nil
		}
	})
}

func qualifyColumns(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("qualify columns")
	tables := make(map[string]sql.Node)
	tableAliases := make(map[string]string)
	colIndex := make(map[string][]string)

	indexCols := func(table string, schema sql.Schema) {
		for _, col := range schema {
			colIndex[col.Name] = append(colIndex[col.Name], table)
		}
	}

	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		switch n := n.(type) {
		case *plan.TableAlias:
			switch t := n.Child.(type) {
			case sql.Table:
				tableAliases[n.Name()] = t.Name()
			default:
				tables[n.Name()] = n.Child
				indexCols(n.Name(), n.Schema())
			}
		case sql.Table:
			tables[n.Name()] = n
			indexCols(n.Name(), n.Schema())
		}

		return n.TransformExpressionsUp(func(e sql.Expression) (sql.Expression, error) {
			a.Log("transforming expression of type: %T", e)
			col, ok := e.(*expression.UnresolvedColumn)
			if !ok {
				return e, nil
			}

			col = expression.NewUnresolvedQualifiedColumn(col.Table(), col.Name())

			if col.Table() == "" {
				tables := dedupStrings(colIndex[col.Name()])
				switch len(tables) {
				case 0:
					return nil, ErrColumnTableNotFound.New(col.Table(), col.Name())
				case 1:
					col = expression.NewUnresolvedQualifiedColumn(
						tables[0],
						col.Name(),
					)
				default:
					return nil, ErrAmbiguousColumnName.New(col.Name(), strings.Join(tables, ", "))
				}
			} else {
				if real, ok := tableAliases[col.Table()]; ok {
					col = expression.NewUnresolvedQualifiedColumn(
						real,
						col.Name(),
					)
				}

				if _, ok := tables[col.Table()]; !ok {
					return nil, sql.ErrTableNotFound.New(col.Table())
				}
			}

			a.Log("column %q was qualified with table %q", col.Name(), col.Table())
			return col, nil
		})
	})
}

func resolveDatabase(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("resolve database, node of type: %T", n)

	// TODO Database should implement node,
	// and ShowTables and CreateTable nodes should be binaryNodes
	switch v := n.(type) {
	case *plan.ShowTables:
		db, err := a.Catalog.Database(a.CurrentDatabase)
		if err != nil {
			return n, err
		}

		v.Database = db
	case *plan.CreateTable:
		db, err := a.Catalog.Database(a.CurrentDatabase)
		if err != nil {
			return n, err
		}

		v.Database = db
	}

	return n, nil
}

func resolveTables(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("resolve table, node of type: %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		t, ok := n.(*plan.UnresolvedTable)
		if !ok {
			return n, nil
		}

		rt, err := a.Catalog.Table(a.CurrentDatabase, t.Name)
		if err != nil {
			return nil, err
		}

		a.Log("table resolved: %q", rt.Name())

		return rt, nil
	})
}

func resolveStar(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("resolving star, node of type: %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		p, ok := n.(*plan.Project)
		if !ok {
			return n, nil
		}

		if len(p.Expressions) != 1 {
			return n, nil
		}

		if _, ok := p.Expressions[0].(*expression.Star); !ok {
			return n, nil
		}

		var exprs []sql.Expression
		for i, e := range p.Child.Schema() {
			gf := expression.NewGetField(i, e.Type, e.Name, e.Nullable)
			exprs = append(exprs, gf)
		}

		a.Log("star replace with %d fields", len(exprs))

		return plan.NewProject(exprs, p.Child), nil
	})
}

type columnInfo struct {
	idx int
	col *sql.Column
}

func resolveColumns(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("resolve columns, node of type: %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		colMap := make(map[string][]columnInfo)
		idx := 0
		for _, child := range n.Children() {
			if !child.Resolved() {
				return n, nil
			}

			for _, col := range child.Schema() {
				colMap[col.Name] = append(colMap[col.Name], columnInfo{idx, col})
				idx++
			}
		}

		return n.TransformExpressionsUp(func(e sql.Expression) (sql.Expression, error) {
			a.Log("transforming expression of type: %T", e)
			if n.Resolved() {
				return e, nil
			}

			uc, ok := e.(*expression.UnresolvedColumn)
			if !ok {
				return e, nil
			}

			columnsInfo, ok := colMap[uc.Name()]
			if !ok {
				return nil, ErrColumnTableNotFound.New(uc.Table(), uc.Name())
			}

			var ci columnInfo
			var found bool
			for _, c := range columnsInfo {
				if c.col.Source == uc.Table() {
					ci = c
					found = true
					break
				}
			}

			if !found {
				return nil, ErrColumnTableNotFound.New(uc.Table(), uc.Name())
			}

			a.Log("column resolved to %q.%q", ci.col.Source, ci.col.Name)

			return expression.NewGetFieldWithTable(
				ci.idx,
				ci.col.Type,
				ci.col.Source,
				ci.col.Name,
				ci.col.Nullable,
			), nil
		})
	})
}

func resolveFunctions(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("resolve functions, node of type %T", n)
	return n.TransformUp(func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		return n.TransformExpressionsUp(func(e sql.Expression) (sql.Expression, error) {
			a.Log("transforming expression of type: %T", e)
			if e.Resolved() {
				return e, nil
			}

			uf, ok := e.(*expression.UnresolvedFunction)
			if !ok {
				return e, nil
			}

			n := uf.Name()
			f, err := a.Catalog.Function(n)
			if err != nil {
				return nil, err
			}

			rf, err := f.Call(uf.Children...)
			if err != nil {
				return nil, err
			}

			a.Log("resolved function %q", n)

			return rf, nil
		})
	})
}

func optimizeDistinct(a *Analyzer, node sql.Node) (sql.Node, error) {
	a.Log("optimize distinct, node of type: %T", node)
	if node, ok := node.(*plan.Distinct); ok {
		var isSorted bool
		_, _ = node.TransformUp(func(node sql.Node) (sql.Node, error) {
			a.Log("checking for optimization in node of type: %T", node)
			if _, ok := node.(*plan.Sort); ok {
				isSorted = true
			}
			return node, nil
		})

		if isSorted {
			a.Log("distinct optimized for ordered output")
			return plan.NewOrderedDistinct(node.Child), nil
		}
	}

	return node, nil
}

func dedupStrings(in []string) []string {
	var seen = make(map[string]struct{})
	var result []string
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			result = append(result, s)
		}
	}
	return result
}

func pushdown(a *Analyzer, n sql.Node) (sql.Node, error) {
	a.Log("pushdown, node of type: %T", n)
	if !n.Resolved() {
		return n, nil
	}

	var fieldsByTable = make(map[string][]string)
	var exprsByTable = make(map[string][]sql.Expression)
	type tableField struct {
		table string
		field string
	}
	var tableFields = make(map[tableField]struct{})

	a.Log("finding used columns in node")

	// First step is to find all col exprs and group them by the table they mention.
	// Even if they appear multiple times, only the first one will be used.
	_, _ = n.TransformExpressionsUp(func(e sql.Expression) (sql.Expression, error) {
		if e, ok := e.(*expression.GetField); ok {
			tf := tableField{e.Table(), e.Name()}
			if _, ok := tableFields[tf]; !ok {
				a.Log("found used column %s.%s", e.Table(), e.Name())
				tableFields[tf] = struct{}{}
				fieldsByTable[e.Table()] = append(fieldsByTable[e.Table()], e.Name())
				exprsByTable[e.Table()] = append(exprsByTable[e.Table()], e)
			}
		}
		return e, nil
	})

	a.Log("finding filters in node")

	// then find all filters, also by table. Note that filters that mention
	// more than one table will not be passed to neither.
	filters := make(filters)
	node, err := n.TransformUp(func(node sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", node)
		switch node := node.(type) {
		case *plan.Filter:
			fs := exprToTableFilters(node.Expression)
			a.Log("found filters for %d tables %s", len(fs), node.Expression)
			filters.merge(fs)
		}

		return node, nil
	})
	if err != nil {
		return nil, err
	}

	a.Log("transforming nodes with pushdown of filters and projections")

	// Now all nodes can be transformed. Since traversal of the tree is done
	// from inner to outer the filters have to be processed first so they get
	// to the tables.
	var handledFilters []sql.Expression
	return node.TransformUp(func(node sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", node)
		switch node := node.(type) {
		case *plan.Filter:
			if len(handledFilters) == 0 {
				a.Log("no handled filters, leaving filter untouched")
				return node, nil
			}

			unhandled := getUnhandledFilters(
				splitExpression(node.Expression),
				handledFilters,
			)

			if len(unhandled) == 0 {
				a.Log("filter node has no unhandled filters, so it will be removed")
				return node.Child, nil
			}

			a.Log(
				"%d handled filters removed from filter node, filter has now %d filters",
				len(handledFilters),
				len(unhandled),
			)

			return plan.NewFilter(expression.JoinAnd(unhandled...), node.Child), nil
		case *plan.PushdownProjectionAndFiltersTable, *plan.PushdownProjectionTable:
			// they also implement the interfaces for pushdown, so we better return
			// or there will be a very nice infinite loop
			return node, nil
		case sql.PushdownProjectionAndFiltersTable:
			cols := exprsByTable[node.Name()]
			tableFilters := filters[node.Name()]
			handled := node.HandledFilters(tableFilters)
			handledFilters = append(handledFilters, handled...)

			a.Log(
				"table %q transformed with pushdown of projection and filters, %d filters handled of %d",
				node.Name(),
				len(handled),
				len(tableFilters),
			)

			schema := node.Schema()
			cols, err := fixFieldIndexesOnExpressions(schema, cols...)
			if err != nil {
				return nil, err
			}

			handled, err = fixFieldIndexesOnExpressions(schema, handled...)
			if err != nil {
				return nil, err
			}

			return plan.NewPushdownProjectionAndFiltersTable(
				cols,
				handled,
				node,
			), nil
		case sql.PushdownProjectionTable:
			cols := fieldsByTable[node.Name()]
			a.Log("table %q transformed with pushdown of projection", node.Name())
			return plan.NewPushdownProjectionTable(cols, node), nil
		}
		return node, nil
	})
}

// fixFieldIndexesOnExpressions executes fixFieldIndexes on a list of exprs.
func fixFieldIndexesOnExpressions(schema sql.Schema, expressions ...sql.Expression) ([]sql.Expression, error) {
	var result = make([]sql.Expression, len(expressions))
	for i, e := range expressions {
		var err error
		result[i], err = fixFieldIndexes(schema, e)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// fixFieldIndexes transforms the given expression setting correct indexes
// for GetField expressions according to the schema of the row in the table
// and not the one where the filter came from.
func fixFieldIndexes(schema sql.Schema, exp sql.Expression) (sql.Expression, error) {
	return exp.TransformUp(func(e sql.Expression) (sql.Expression, error) {
		switch e := e.(type) {
		case *expression.GetField:
			// we need to rewrite the indexes for the table row
			for i, col := range schema {
				if e.Name() == col.Name {
					return expression.NewGetFieldWithTable(
						i,
						e.Type(),
						e.Table(),
						e.Name(),
						e.IsNullable(),
					), nil
				}
			}

			return nil, ErrFieldMissing.New(e.Name())
		}

		return e, nil
	})
}