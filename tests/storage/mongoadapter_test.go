package models

import (
	"testing"

	"github.com/go-behemoth/behemoth/clause"
	mongoAdapter "github.com/go-behemoth/behemoth/storage/adapters/mongo"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

func TestBuildMongoFilter(t *testing.T) {
	tests := []struct {
		name     string
		expr     *clause.Expression
		expected bson.M
	}{
		{
			name:     "nil expression returns empty filter",
			expr:     nil,
			expected: bson.M{},
		},
		{
			name: "single condition - eq",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "name", Operator: clause.OpEqual, Value: "john"},
				},
			},
			expected: bson.M{"name": "john"},
		},
		{
			name: "single condition - ne",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "age", Operator: clause.OpNotEqual, Value: 30},
				},
			},
			expected: bson.M{"age": bson.M{"$ne": 30}},
		},
		{
			name: "single condition - gt",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "age", Operator: clause.OpGreaterThan, Value: 18},
				},
			},
			expected: bson.M{"age": bson.M{"$gt": 18}},
		},
		{
			name: "single condition - gte",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "age", Operator: clause.OpGreaterEq, Value: 18},
				},
			},
			expected: bson.M{"age": bson.M{"$gte": 18}},
		},
		{
			name: "single condition - lt",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "age", Operator: clause.OpLessThan, Value: 65},
				},
			},
			expected: bson.M{"age": bson.M{"$lt": 65}},
		},
		{
			name: "single condition - lte",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "age", Operator: clause.OpLessEq, Value: 65},
				},
			},
			expected: bson.M{"age": bson.M{"$lte": 65}},
		},
		{
			name: "single condition - in",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "status", Operator: clause.OpIn, Value: []string{"active", "pending"}},
				},
			},
			expected: bson.M{"status": bson.M{"$in": []any{"active", "pending"}}},
		},
		{
			name: "single condition - nin",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "status", Operator: clause.OpNotIn, Value: []string{"deleted", "archived"}},
				},
			},
			expected: bson.M{"status": bson.M{"$nin": []any{"deleted", "archived"}}},
		},
		{
			name: "single condition - contains",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "description", Operator: clause.OpContains, Value: "test"},
				},
			},
			expected: bson.M{"description": bson.M{"$regex": "test"}},
		},
		{
			name: "single condition - starts_with",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "name", Operator: clause.OpStartsWith, Value: "john"},
				},
			},
			expected: bson.M{"name": bson.M{"$regex": "^john"}},
		},
		{
			name: "single condition - ends_with",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "email", Operator: clause.OpEndsWith, Value: "@gmail.com"},
				},
			},
			expected: bson.M{"email": bson.M{"$regex": "@gmail.com$"}},
		},
		{
			name: "single condition - is_null",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "deleted_at", Operator: clause.OpIsNull, Value: nil},
				},
			},
			expected: bson.M{"deleted_at": nil},
		},
		{
			name: "single condition - not_null",
			expr: &clause.Expression{
				Conditions: []clause.Condition{
					{Field: "email", Operator: clause.OpNotNull, Value: nil},
				},
			},
			expected: bson.M{"email": bson.M{"$ne": nil}},
		},
		{
			name: "multiple conditions with AND logic",
			expr: &clause.Expression{
				Logic: clause.OpAnd,
				Conditions: []clause.Condition{
					{Field: "age", Operator: clause.OpGreaterEq, Value: 18},
					{Field: "status", Operator: clause.OpEqual, Value: "active"},
				},
			},
			expected: bson.M{
				"$and": []bson.M{
					{"age": bson.M{"$gte": 18}},
					{"status": "active"},
				},
			},
		},
		{
			name: "multiple conditions with OR logic",
			expr: &clause.Expression{
				Logic: clause.OpOr,
				Conditions: []clause.Condition{
					{Field: "status", Operator: clause.OpEqual, Value: "pending"},
					{Field: "status", Operator: clause.OpEqual, Value: "active"},
				},
			},
			expected: bson.M{
				"$or": []bson.M{
					{"status": "pending"},
					{"status": "active"},
				},
			},
		},
		{
			name: "single child expression",
			expr: &clause.Expression{
				Logic: clause.OpAnd,
				Children: []*clause.Expression{
					{
						Conditions: []clause.Condition{
							{Field: "age", Operator: clause.OpGreaterEq, Value: 18},
						},
					},
				},
			},
			expected: bson.M{"age": bson.M{"$gte": 18}},
		},
		{
			name: "nested expressions with AND and OR",
			expr: &clause.Expression{
				Logic: clause.OpAnd,
				Conditions: []clause.Condition{
					{Field: "active", Operator: clause.OpEqual, Value: true},
				},
				Children: []*clause.Expression{
					{
						Logic: clause.OpOr,
						Conditions: []clause.Condition{
							{Field: "role", Operator: clause.OpEqual, Value: "admin"},
							{Field: "role", Operator: clause.OpEqual, Value: "moderator"},
						},
					},
				},
			},
			expected: bson.M{
				"$and": []bson.M{
					{
						"$or": []bson.M{
							{"role": "admin"},
							{"role": "moderator"},
						},
					},
					{"active": true},
				},
			},
		},
		{
			name: "deeply nested expressions",
			expr: &clause.Expression{
				Logic: clause.OpAnd,
				Children: []*clause.Expression{
					{
						Logic: clause.OpOr,
						Children: []*clause.Expression{
							{
								Conditions: []clause.Condition{
									{Field: "priority", Operator: clause.OpGreaterThan, Value: 5},
								},
							},
							{
								Conditions: []clause.Condition{
									{Field: "urgent", Operator: clause.OpEqual, Value: true},
								},
							},
						},
					},
					{
						Conditions: []clause.Condition{
							{Field: "status", Operator: clause.OpNotEqual, Value: "archived"},
						},
					},
				},
			},
			expected: bson.M{
				"$and": []bson.M{
					{
						"$or": []bson.M{
							{"priority": bson.M{"$gt": 5}},
							{"urgent": true},
						},
					},
					{"status": bson.M{"$ne": "archived"}},
				},
			},
		},
		{
			name: "empty conditions and no children",
			expr: &clause.Expression{
				Logic:      clause.OpAnd,
				Conditions: []clause.Condition{},
				Children:   []*clause.Expression{},
			},
			expected: bson.M{},
		},
		{
			name: "complex filter with mixed data types",
			expr: &clause.Expression{
				Logic: clause.OpAnd,
				Conditions: []clause.Condition{
					{Field: "age", Operator: clause.OpGreaterEq, Value: 18},
					{Field: "tags", Operator: clause.OpIn, Value: []string{"premium", "vip"}},
					{Field: "name", Operator: clause.OpContains, Value: "john"},
				},
			},
			expected: bson.M{
				"$and": []bson.M{
					{"age": bson.M{"$gte": 18}},
					{"tags": bson.M{"$in": []any{"premium", "vip"}}},
					{"name": bson.M{"$regex": "john"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mongoAdapter.BuildMongoFilter(tt.expr)
			assert.Equal(t, tt.expected, result)
		})
	}

}

func TestBuildMongoFilterEdgeCases(t *testing.T) {
	t.Run("empty values in conditions", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "name", Operator: clause.OpEqual, Value: ""},
				{Field: "age", Operator: clause.OpEqual, Value: 0},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{
			"$and": []bson.M{
				{"name": ""},
				{"age": 0},
			},
		}
		assert.Equal(t, expected, result)
	})

	t.Run("nil values in conditions", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "deleted_at", Operator: clause.OpIsNull, Value: nil},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{"deleted_at": nil}
		assert.Equal(t, expected, result)
	})

	t.Run("empty slice in IN operator", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "ids", Operator: clause.OpIn, Value: []string{}},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{"ids": bson.M{"$in": []any{}}}
		assert.Equal(t, expected, result)
	})
	t.Run("non-slice in IN operator", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "ids", Operator: clause.OpIn, Value: 1},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{"ids": bson.M{"$in": []any{1}}}
		assert.Equal(t, expected, result)
	})
	t.Run("non-slice in NOT-IN operator", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "ids", Operator: clause.OpNotIn, Value: 1},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{"ids": bson.M{"$nin": []any{1}}}
		assert.Equal(t, expected, result)
	})
	t.Run("multiple children with same level", func(t *testing.T) {
		expr := &clause.Expression{
			Logic: clause.OpOr,
			Children: []*clause.Expression{
				{
					Conditions: []clause.Condition{
						{Field: "a", Operator: clause.OpEqual, Value: 1},
					},
				},
				{
					Conditions: []clause.Condition{
						{Field: "b", Operator: clause.OpEqual, Value: 2},
					},
				},
				{
					Conditions: []clause.Condition{
						{Field: "c", Operator: clause.OpEqual, Value: 3},
					},
				},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{
			"$or": []bson.M{
				{"a": 1},
				{"b": 2},
				{"c": 3},
			},
		}
		assert.Equal(t, expected, result)
	})

	t.Run("mixed children and conditions", func(t *testing.T) {
		expr := &clause.Expression{
			Logic: clause.OpAnd,
			Conditions: []clause.Condition{
				{Field: "active", Operator: clause.OpEqual, Value: true},
			},
			Children: []*clause.Expression{
				{
					Conditions: []clause.Condition{
						{Field: "score", Operator: clause.OpGreaterThan, Value: 50},
					},
				},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{
			"$and": []bson.M{
				{"score": bson.M{"$gt": 50}},
				{"active": true},
			},
		}
		assert.Equal(t, expected, result)
	})
}

func TestBuildMongoFilterUnsupportedOperator(t *testing.T) {
	t.Run("unsupported operator returns empty bson.M", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "test", Operator: "unsupported", Value: "value"},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{}
		assert.Equal(t, expected, result)
	})
}

func TestBuildMongoFilterNilValueHandling(t *testing.T) {
	t.Run("condition with nil value for eq", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "field", Operator: clause.OpEqual, Value: nil},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{"field": nil}
		assert.Equal(t, expected, result)
	})

	t.Run("condition with nil value for ne", func(t *testing.T) {
		expr := &clause.Expression{
			Conditions: []clause.Condition{
				{Field: "field", Operator: clause.OpNotEqual, Value: nil},
			},
		}
		result := mongoAdapter.BuildMongoFilter(expr)
		expected := bson.M{"field": bson.M{"$ne": nil}}
		assert.Equal(t, expected, result)
	})
}
