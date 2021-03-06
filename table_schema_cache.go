package ghostferry

import (
	"database/sql"
	"fmt"

	sq "github.com/Masterminds/squirrel"
	"github.com/siddontang/go-mysql/schema"
	"github.com/sirupsen/logrus"
)

var ignoredDatabases = map[string]bool{
	"mysql":              true,
	"information_schema": true,
	"performance_schema": true,
	"sys":                true,
}

type TableSchemaCache map[string]*schema.Table

func QuotedTableName(table *schema.Table) string {
	return QuotedTableNameFromString(table.Schema, table.Name)
}

func QuotedTableNameFromString(database, table string) string {
	return fmt.Sprintf("`%s`.`%s`", database, table)
}

func MaxPrimaryKeys(db *sql.DB, tables []*schema.Table, logger *logrus.Entry) (map[*schema.Table]uint64, []*schema.Table, error) {
	tablesWithData := make(map[*schema.Table]uint64)
	emptyTables := make([]*schema.Table, 0, len(tables))

	for _, table := range tables {
		logger := logger.WithField("table", table.String())

		maxPk, maxPkExists, err := maxPk(db, table)
		if err != nil {
			logger.WithError(err).Errorf("failed to get max primary key %s", table.GetPKColumn(0).Name)
			return tablesWithData, emptyTables, err
		}

		if !maxPkExists {
			emptyTables = append(emptyTables, table)
			logger.Warn("no data in this table, skipping")
			continue
		}

		tablesWithData[table] = maxPk
	}

	return tablesWithData, emptyTables, nil
}

func LoadTables(db *sql.DB, tableFilter TableFilter) (TableSchemaCache, error) {
	logger := logrus.WithField("tag", "table_schema_cache")

	tableSchemaCache := make(TableSchemaCache)

	dbnames, err := showDatabases(db)
	if err != nil {
		logger.WithError(err).Error("failed to show databases")
		return tableSchemaCache, err
	}

	dbnames, err = tableFilter.ApplicableDatabases(dbnames)
	if err != nil {
		logger.WithError(err).Error("could not apply database filter")
		return tableSchemaCache, err
	}

	// For each database, get a list of tables from it and cache the table's schema
	for _, dbname := range dbnames {
		dbLog := logger.WithField("database", dbname)
		dbLog.Debug("loading tables from database")
		tableNames, err := showTablesFrom(db, dbname)
		if err != nil {
			dbLog.WithError(err).Error("failed to show tables")
			return tableSchemaCache, err
		}

		var tableSchemas []*schema.Table

		for _, table := range tableNames {
			tableLog := dbLog.WithField("table", table)
			tableLog.Debug("fetching table schema")
			tableSchema, err := schema.NewTableFromSqlDB(db, dbname, table)
			if err != nil {
				tableLog.WithError(err).Error("cannot fetch table schema from source db")
				return tableSchemaCache, err
			}
			tableSchemas = append(tableSchemas, tableSchema)
		}

		tableSchemas, err = tableFilter.ApplicableTables(tableSchemas)
		if err != nil {
			return tableSchemaCache, nil
		}

		for _, tableSchema := range tableSchemas {
			tableName := tableSchema.Name
			tableLog := dbLog.WithField("table", tableName)
			tableLog.Debug("caching table schema")

			// Sanity check
			if len(tableSchema.PKColumns) != 1 {
				err = fmt.Errorf("table %s has %d primary key columns and this is not supported", tableName, len(tableSchema.PKColumns))
				logger.WithError(err).Error("invalid table")
				return tableSchemaCache, err
			}

			if tableSchema.GetPKColumn(0).Type != schema.TYPE_NUMBER {
				err = fmt.Errorf("table %s is using a non-numeric primary key column and this is not supported", tableName)
				logger.WithError(err).Error("invalid table")
				return tableSchemaCache, err
			}

			tableSchemaCache[tableSchema.String()] = tableSchema
		}
	}

	logger.WithField("tables", tableSchemaCache.AllTableNames()).Info("table schemas cached")

	return tableSchemaCache, nil
}

func (c TableSchemaCache) AsSlice() (tables []*schema.Table) {
	for _, tableSchema := range c {
		tables = append(tables, tableSchema)
	}

	return
}

func (c TableSchemaCache) AllTableNames() (tableNames []string) {
	for tableName, _ := range c {
		tableNames = append(tableNames, tableName)
	}

	return
}

func (c TableSchemaCache) Get(database, table string) *schema.Table {
	fullTableName := fmt.Sprintf("%s.%s", database, table)
	return c[fullTableName]
}

func showDatabases(c *sql.DB) ([]string, error) {
	rows, err := c.Query("show databases")
	if err != nil {
		return []string{}, err
	}

	defer rows.Close()

	databases := make([]string, 0)
	for rows.Next() {
		var database string
		err = rows.Scan(&database)
		if err != nil {
			return databases, err
		}

		if _, ignored := ignoredDatabases[database]; ignored {
			continue
		}

		databases = append(databases, database)
	}

	return databases, nil
}

func showTablesFrom(c *sql.DB, dbname string) ([]string, error) {
	rows, err := c.Query(fmt.Sprintf("show tables from %s", quoteField(dbname)))
	if err != nil {
		return []string{}, err
	}
	defer rows.Close()

	tables := make([]string, 0)
	for rows.Next() {
		var table string
		err = rows.Scan(&table)
		if err != nil {
			return tables, err
		}

		tables = append(tables, table)
	}

	return tables, nil
}

func maxPk(db *sql.DB, table *schema.Table) (uint64, bool, error) {
	primaryKeyColumn := table.GetPKColumn(0)
	pkName := quoteField(primaryKeyColumn.Name)

	query, args, err := sq.
		Select(pkName).
		From(QuotedTableName(table)).
		OrderBy(fmt.Sprintf("%s DESC", pkName)).
		Limit(1).
		ToSql()

	if err != nil {
		return 0, false, err
	}

	var maxPrimaryKey uint64
	err = db.QueryRow(query, args...).Scan(&maxPrimaryKey)

	switch {
	case err == sql.ErrNoRows:
		return 0, false, nil
	case err != nil:
		return 0, false, err
	default:
		return maxPrimaryKey, true, nil
	}
}
