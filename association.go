package gorm

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
	"gorm.io/gorm/utils"
)

// Association Mode contains some helper methods to handle relationship things easily.
type Association struct {
	DB           *DB
	Relationship *schema.Relationship
	Error        error
}

func (db *DB) Association(column string) *Association {
	association := &Association{DB: db}
	table := db.Statement.Table

	if err := db.Statement.Parse(db.Statement.Model); err == nil {
		db.Statement.Table = table
		association.Relationship = db.Statement.Schema.Relationships.Relations[column]

		if association.Relationship == nil {
			association.Error = fmt.Errorf("%w: %v", ErrUnsupportedRelation, column)
		}

		db.Statement.ReflectValue = reflect.Indirect(reflect.ValueOf(db.Statement.Model))
	} else {
		association.Error = err
	}

	return association
}

func (association *Association) Find(out interface{}, conds ...interface{}) error {
	if association.Error == nil {
		var (
			queryConds = association.Relationship.ToQueryConditions(association.DB.Statement.ReflectValue)
			tx         = association.DB.Model(out)
		)

		if association.Relationship.JoinTable != nil {
			if !tx.Statement.Unscoped && len(association.Relationship.JoinTable.QueryClauses) > 0 {
				joinStmt := Statement{DB: tx, Schema: association.Relationship.JoinTable, Table: association.Relationship.JoinTable.Table, Clauses: map[string]clause.Clause{}}
				for _, queryClause := range association.Relationship.JoinTable.QueryClauses {
					joinStmt.AddClause(queryClause)
				}
				joinStmt.Build("WHERE", "LIMIT")
				tx.Clauses(clause.Expr{SQL: strings.Replace(joinStmt.SQL.String(), "WHERE ", "", 1), Vars: joinStmt.Vars})
			}

			tx.Clauses(clause.From{Joins: []clause.Join{{
				Table: clause.Table{Name: association.Relationship.JoinTable.Table},
				ON:    clause.Where{Exprs: queryConds},
			}}})
		} else {
			tx.Clauses(clause.Where{Exprs: queryConds})
		}

		association.Error = tx.Find(out, conds...).Error
	}

	return association.Error
}

func (association *Association) Append(values ...interface{}) error {
	if association.Error == nil {
		switch association.Relationship.Type {
		case schema.HasOne, schema.BelongsTo:
			if len(values) > 0 {
				association.Error = association.Replace(values...)
			}
		default:
			association.saveAssociation(false, values...)
		}
	}

	return association.Error
}

func (association *Association) Replace(values ...interface{}) error {
	if association.Error == nil {
		// save associations
		association.saveAssociation(true, values...)

		// set old associations's foreign key to null
		reflectValue := association.DB.Statement.ReflectValue
		rel := association.Relationship
		switch rel.Type {
		case schema.BelongsTo:
			if len(values) == 0 {
				updateMap := map[string]interface{}{}
				switch reflectValue.Kind() {
				case reflect.Slice, reflect.Array:
					for i := 0; i < reflectValue.Len(); i++ {
						rel.Field.Set(reflectValue.Index(i), reflect.Zero(rel.Field.FieldType).Interface())
					}
				case reflect.Struct:
					rel.Field.Set(reflectValue, reflect.Zero(rel.Field.FieldType).Interface())
				}

				for _, ref := range rel.References {
					updateMap[ref.ForeignKey.DBName] = nil
				}

				association.DB.UpdateColumns(updateMap)
			}
		case schema.HasOne, schema.HasMany:
			var (
				primaryFields []*schema.Field
				foreignKeys   []string
				updateMap     = map[string]interface{}{}
				relValues     = schema.GetRelationsValues(reflectValue, []*schema.Relationship{rel})
				modelValue    = reflect.New(rel.FieldSchema.ModelType).Interface()
				tx            = association.DB.Model(modelValue)
			)

			if _, rvs := schema.GetIdentityFieldValuesMap(relValues, rel.FieldSchema.PrimaryFields); len(rvs) > 0 {
				if column, values := schema.ToQueryValues(rel.FieldSchema.Table, rel.FieldSchema.PrimaryFieldDBNames, rvs); len(values) > 0 {
					tx.Not(clause.IN{Column: column, Values: values})
				}
			}

			for _, ref := range rel.References {
				if ref.OwnPrimaryKey {
					primaryFields = append(primaryFields, ref.PrimaryKey)
					foreignKeys = append(foreignKeys, ref.ForeignKey.DBName)
					updateMap[ref.ForeignKey.DBName] = nil
				} else if ref.PrimaryValue != "" {
					tx.Where(clause.Eq{Column: ref.ForeignKey.DBName, Value: ref.PrimaryValue})
				}
			}

			if _, pvs := schema.GetIdentityFieldValuesMap(reflectValue, primaryFields); len(pvs) > 0 {
				column, values := schema.ToQueryValues(rel.FieldSchema.Table, foreignKeys, pvs)
				tx.Where(clause.IN{Column: column, Values: values}).UpdateColumns(updateMap)
			}
		case schema.Many2Many:
			var (
				primaryFields, relPrimaryFields     []*schema.Field
				joinPrimaryKeys, joinRelPrimaryKeys []string
				modelValue                          = reflect.New(rel.JoinTable.ModelType).Interface()
				tx                                  = association.DB.Model(modelValue)
			)

			for _, ref := range rel.References {
				if ref.PrimaryValue == "" {
					if ref.OwnPrimaryKey {
						primaryFields = append(primaryFields, ref.PrimaryKey)
						joinPrimaryKeys = append(joinPrimaryKeys, ref.ForeignKey.DBName)
					} else {
						relPrimaryFields = append(relPrimaryFields, ref.PrimaryKey)
						joinRelPrimaryKeys = append(joinRelPrimaryKeys, ref.ForeignKey.DBName)
					}
				} else {
					tx.Clauses(clause.Eq{Column: ref.ForeignKey.DBName, Value: ref.PrimaryValue})
				}
			}

			_, pvs := schema.GetIdentityFieldValuesMap(reflectValue, primaryFields)
			if column, values := schema.ToQueryValues(rel.JoinTable.Table, joinPrimaryKeys, pvs); len(values) > 0 {
				tx.Where(clause.IN{Column: column, Values: values})
			} else {
				return ErrorPrimaryKeyRequired
			}

			_, rvs := schema.GetIdentityFieldValuesMapFromValues(values, relPrimaryFields)
			if relColumn, relValues := schema.ToQueryValues(rel.JoinTable.Table, joinRelPrimaryKeys, rvs); len(relValues) > 0 {
				tx.Where(clause.Not(clause.IN{Column: relColumn, Values: relValues}))
			}

			tx.Delete(modelValue)
		}
	}
	return association.Error
}

func (association *Association) Delete(values ...interface{}) error {
	if association.Error == nil {
		var (
			reflectValue                 = association.DB.Statement.ReflectValue
			rel                          = association.Relationship
			primaryFields, foreignFields []*schema.Field
			foreignKeys                  []string
			updateAttrs                  = map[string]interface{}{}
			conds                        []clause.Expression
		)

		for _, ref := range rel.References {
			if ref.PrimaryValue == "" {
				primaryFields = append(primaryFields, ref.PrimaryKey)
				foreignFields = append(foreignFields, ref.ForeignKey)
				foreignKeys = append(foreignKeys, ref.ForeignKey.DBName)
				updateAttrs[ref.ForeignKey.DBName] = nil
			} else {
				conds = append(conds, clause.Eq{Column: ref.ForeignKey.DBName, Value: ref.PrimaryValue})
			}
		}

		switch rel.Type {
		case schema.BelongsTo:
			tx := association.DB.Model(reflect.New(rel.Schema.ModelType).Interface())

			_, pvs := schema.GetIdentityFieldValuesMap(reflectValue, rel.Schema.PrimaryFields)
			pcolumn, pvalues := schema.ToQueryValues(rel.Schema.Table, rel.Schema.PrimaryFieldDBNames, pvs)
			conds = append(conds, clause.IN{Column: pcolumn, Values: pvalues})

			_, rvs := schema.GetIdentityFieldValuesMapFromValues(values, primaryFields)
			relColumn, relValues := schema.ToQueryValues(rel.Schema.Table, foreignKeys, rvs)
			conds = append(conds, clause.IN{Column: relColumn, Values: relValues})

			association.Error = tx.Clauses(conds...).UpdateColumns(updateAttrs).Error
		case schema.HasOne, schema.HasMany:
			tx := association.DB.Model(reflect.New(rel.FieldSchema.ModelType).Interface())

			_, pvs := schema.GetIdentityFieldValuesMap(reflectValue, primaryFields)
			pcolumn, pvalues := schema.ToQueryValues(rel.FieldSchema.Table, foreignKeys, pvs)
			conds = append(conds, clause.IN{Column: pcolumn, Values: pvalues})

			_, rvs := schema.GetIdentityFieldValuesMapFromValues(values, rel.FieldSchema.PrimaryFields)
			relColumn, relValues := schema.ToQueryValues(rel.FieldSchema.Table, rel.FieldSchema.PrimaryFieldDBNames, rvs)
			conds = append(conds, clause.IN{Column: relColumn, Values: relValues})

			association.Error = tx.Clauses(conds...).UpdateColumns(updateAttrs).Error
		case schema.Many2Many:
			var (
				primaryFields, relPrimaryFields     []*schema.Field
				joinPrimaryKeys, joinRelPrimaryKeys []string
				modelValue                          = reflect.New(rel.JoinTable.ModelType).Interface()
			)

			for _, ref := range rel.References {
				if ref.PrimaryValue == "" {
					if ref.OwnPrimaryKey {
						primaryFields = append(primaryFields, ref.PrimaryKey)
						joinPrimaryKeys = append(joinPrimaryKeys, ref.ForeignKey.DBName)
					} else {
						relPrimaryFields = append(relPrimaryFields, ref.PrimaryKey)
						joinRelPrimaryKeys = append(joinRelPrimaryKeys, ref.ForeignKey.DBName)
					}
				} else {
					conds = append(conds, clause.Eq{Column: ref.ForeignKey.DBName, Value: ref.PrimaryValue})
				}
			}

			_, pvs := schema.GetIdentityFieldValuesMap(reflectValue, primaryFields)
			pcolumn, pvalues := schema.ToQueryValues(rel.JoinTable.Table, joinPrimaryKeys, pvs)
			conds = append(conds, clause.IN{Column: pcolumn, Values: pvalues})

			_, rvs := schema.GetIdentityFieldValuesMapFromValues(values, relPrimaryFields)
			relColumn, relValues := schema.ToQueryValues(rel.JoinTable.Table, joinRelPrimaryKeys, rvs)
			conds = append(conds, clause.IN{Column: relColumn, Values: relValues})

			association.Error = association.DB.Where(clause.Where{Exprs: conds}).Model(nil).Delete(modelValue).Error
		}

		if association.Error == nil {
			relValuesMap, _ := schema.GetIdentityFieldValuesMapFromValues(values, rel.FieldSchema.PrimaryFields)

			cleanUpDeletedRelations := func(data reflect.Value) {
				if _, zero := rel.Field.ValueOf(data); !zero {
					fieldValue := reflect.Indirect(rel.Field.ReflectValueOf(data))
					primaryValues := make([]interface{}, len(rel.FieldSchema.PrimaryFields))

					switch fieldValue.Kind() {
					case reflect.Slice, reflect.Array:
						validFieldValues := reflect.Zero(rel.Field.IndirectFieldType)
						for i := 0; i < fieldValue.Len(); i++ {
							for idx, field := range rel.FieldSchema.PrimaryFields {
								primaryValues[idx], _ = field.ValueOf(fieldValue.Index(i))
							}

							if _, ok := relValuesMap[utils.ToStringKey(primaryValues...)]; !ok {
								validFieldValues = reflect.Append(validFieldValues, fieldValue.Index(i))
							}
						}

						rel.Field.Set(data, validFieldValues.Interface())
					case reflect.Struct:
						for idx, field := range rel.FieldSchema.PrimaryFields {
							primaryValues[idx], _ = field.ValueOf(fieldValue)
						}

						if _, ok := relValuesMap[utils.ToStringKey(primaryValues...)]; ok {
							rel.Field.Set(data, reflect.Zero(rel.FieldSchema.ModelType).Interface())

							if rel.JoinTable == nil {
								for _, ref := range rel.References {
									if ref.OwnPrimaryKey || ref.PrimaryValue != "" {
										ref.ForeignKey.Set(fieldValue, reflect.Zero(ref.ForeignKey.FieldType).Interface())
									} else {
										ref.ForeignKey.Set(data, reflect.Zero(ref.ForeignKey.FieldType).Interface())
									}
								}
							}
						}
					}
				}
			}

			switch reflectValue.Kind() {
			case reflect.Slice, reflect.Array:
				for i := 0; i < reflectValue.Len(); i++ {
					cleanUpDeletedRelations(reflect.Indirect(reflectValue.Index(i)))
				}
			case reflect.Struct:
				cleanUpDeletedRelations(reflectValue)
			}
		}
	}

	return association.Error
}

func (association *Association) Clear() error {
	return association.Replace()
}

func (association *Association) Count() (count int64) {
	if association.Error == nil {
		var (
			conds      = association.Relationship.ToQueryConditions(association.DB.Statement.ReflectValue)
			modelValue = reflect.New(association.Relationship.FieldSchema.ModelType).Interface()
			tx         = association.DB.Model(modelValue)
		)

		if association.Relationship.JoinTable != nil {
			if !tx.Statement.Unscoped && len(association.Relationship.JoinTable.QueryClauses) > 0 {
				joinStmt := Statement{DB: tx, Schema: association.Relationship.JoinTable, Table: association.Relationship.JoinTable.Table, Clauses: map[string]clause.Clause{}}
				for _, queryClause := range association.Relationship.JoinTable.QueryClauses {
					joinStmt.AddClause(queryClause)
				}
				joinStmt.Build("WHERE", "LIMIT")
				tx.Clauses(clause.Expr{SQL: strings.Replace(joinStmt.SQL.String(), "WHERE ", "", 1), Vars: joinStmt.Vars})
			}

			tx.Clauses(clause.From{Joins: []clause.Join{{
				Table: clause.Table{Name: association.Relationship.JoinTable.Table},
				ON:    clause.Where{Exprs: conds},
			}}})
		} else {
			tx.Clauses(clause.Where{Exprs: conds})
		}

		association.Error = tx.Count(&count).Error
	}

	return
}

type assignBack struct {
	Source reflect.Value
	Index  int
	Dest   reflect.Value
}

func (association *Association) saveAssociation(clear bool, values ...interface{}) {
	var (
		reflectValue = association.DB.Statement.ReflectValue
		assignBacks  []assignBack // assign association values back to arguments after save
	)

	appendToRelations := func(source, rv reflect.Value, clear bool) {
		switch association.Relationship.Type {
		case schema.HasOne, schema.BelongsTo:
			switch rv.Kind() {
			case reflect.Slice, reflect.Array:
				if rv.Len() > 0 {
					association.Error = association.Relationship.Field.Set(source, rv.Index(0).Addr().Interface())

					if association.Relationship.Field.FieldType.Kind() == reflect.Struct {
						assignBacks = append(assignBacks, assignBack{Source: source, Dest: rv.Index(0)})
					}
				}
			case reflect.Struct:
				association.Error = association.Relationship.Field.Set(source, rv.Addr().Interface())

				if association.Relationship.Field.FieldType.Kind() == reflect.Struct {
					assignBacks = append(assignBacks, assignBack{Source: source, Dest: rv})
				}
			}
		case schema.HasMany, schema.Many2Many:
			elemType := association.Relationship.Field.IndirectFieldType.Elem()
			fieldValue := reflect.Indirect(association.Relationship.Field.ReflectValueOf(source))
			if clear {
				fieldValue = reflect.New(association.Relationship.Field.IndirectFieldType).Elem()
			}

			appendToFieldValues := func(ev reflect.Value) {
				if ev.Type().AssignableTo(elemType) {
					fieldValue = reflect.Append(fieldValue, ev)
				} else if ev.Type().Elem().AssignableTo(elemType) {
					fieldValue = reflect.Append(fieldValue, ev.Elem())
				} else {
					association.Error = fmt.Errorf("unsupported data type: %v for relation %v", ev.Type(), association.Relationship.Name)
				}

				if elemType.Kind() == reflect.Struct {
					assignBacks = append(assignBacks, assignBack{Source: source, Dest: ev, Index: fieldValue.Len()})
				}
			}

			switch rv.Kind() {
			case reflect.Slice, reflect.Array:
				for i := 0; i < rv.Len(); i++ {
					appendToFieldValues(reflect.Indirect(rv.Index(i)).Addr())
				}
			case reflect.Struct:
				appendToFieldValues(rv.Addr())
			}

			if association.Error == nil {
				association.Error = association.Relationship.Field.Set(source, fieldValue.Interface())
			}
		}
	}

	selectedSaveColumns := []string{association.Relationship.Name}
	for _, ref := range association.Relationship.References {
		if !ref.OwnPrimaryKey {
			selectedSaveColumns = append(selectedSaveColumns, ref.ForeignKey.Name)
		}
	}

	switch reflectValue.Kind() {
	case reflect.Slice, reflect.Array:
		if len(values) != reflectValue.Len() {
			if clear && len(values) == 0 {
				for i := 0; i < reflectValue.Len(); i++ {
					association.Relationship.Field.Set(reflectValue.Index(i), reflect.New(association.Relationship.Field.IndirectFieldType).Interface())

					if association.Relationship.JoinTable == nil {
						for _, ref := range association.Relationship.References {
							if !ref.OwnPrimaryKey && ref.PrimaryValue == "" {
								ref.ForeignKey.Set(reflectValue.Index(i), reflect.Zero(ref.ForeignKey.FieldType).Interface())
							}
						}
					}
				}
				break
			}

			association.Error = errors.New("invalid association values, length doesn't match")
			return
		}

		for i := 0; i < reflectValue.Len(); i++ {
			appendToRelations(reflectValue.Index(i), reflect.Indirect(reflect.ValueOf(values[i])), clear)

			// TODO support save slice data, sql with case?
			association.Error = association.DB.Session(&Session{}).Select(selectedSaveColumns).Model(nil).Save(reflectValue.Index(i).Addr().Interface()).Error
		}
	case reflect.Struct:
		if clear && len(values) == 0 {
			association.Relationship.Field.Set(reflectValue, reflect.New(association.Relationship.Field.IndirectFieldType).Interface())

			if association.Relationship.JoinTable == nil {
				for _, ref := range association.Relationship.References {
					if !ref.OwnPrimaryKey && ref.PrimaryValue == "" {
						ref.ForeignKey.Set(reflectValue, reflect.Zero(ref.ForeignKey.FieldType).Interface())
					}
				}
			}
		}

		for idx, value := range values {
			rv := reflect.Indirect(reflect.ValueOf(value))
			appendToRelations(reflectValue, rv, clear && idx == 0)
		}

		if len(values) > 0 {
			association.Error = association.DB.Session(&Session{}).Select(selectedSaveColumns).Model(nil).Save(reflectValue.Addr().Interface()).Error
		}
	}

	for _, assignBack := range assignBacks {
		fieldValue := reflect.Indirect(association.Relationship.Field.ReflectValueOf(assignBack.Source))
		if assignBack.Index > 0 {
			reflect.Indirect(assignBack.Dest).Set(fieldValue.Index(assignBack.Index - 1))
		} else {
			reflect.Indirect(assignBack.Dest).Set(fieldValue)
		}
	}
}
