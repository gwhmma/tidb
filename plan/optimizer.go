// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"github.com/sirupsen/logrus"
	"math"
	"reflect"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/privilege"
	"github.com/pingcap/tidb/sessionctx"
)

// AllowCartesianProduct means whether tidb allows cartesian join without equal conditions.
var AllowCartesianProduct = true

const (
	flagPrunColumns uint64 = 1 << iota
	flagEliminateProjection
	flagBuildKeyInfo
	flagDecorrelate
	flagMaxMinEliminate
	flagPredicatePushDown
	flagAggregationOptimize
	flagPushDownTopN
)

var optRuleList = []logicalOptRule{
	&columnPruner{},
	&projectionEliminater{},
	&buildKeySolver{},
	&decorrelateSolver{},
	&maxMinEliminator{},
	&ppdSolver{},
	&aggregationOptimizer{},
	&pushDownTopNOptimizer{},
}

// logicalOptRule means a logical optimizing rule, which contains decorrelate, ppd, column pruning, etc.
type logicalOptRule interface {
	optimize(LogicalPlan) (LogicalPlan, error)
}

// Optimize does optimization and creates a Plan.
// The node must be prepared first.
func Optimize(ctx sessionctx.Context, node ast.Node, is infoschema.InfoSchema) (Plan, error) {
	logrus.Infof("------------------ step into optimize ")
	ctx.GetSessionVars().PlanID = 0
	builder := &planBuilder{
		ctx:       ctx,
		is:        is,
		colMapper: make(map[*ast.ColumnNameExpr]int),
	}
	logrus.Infof("use planBuilder to build plan")
	// code_analysis 通过不同的ast类型，进入对应分支构建logical plan
	// if insert plan.Insert
	p := builder.build(node)
	if builder.err != nil {
		return nil, errors.Trace(builder.err)
	}
	logrus.Infof("got  plan: %s", reflect.TypeOf(p))

	// code_analysis 这边就是使用visitInfo 去privilege 模块校验用户权限
	// Maybe it's better to move this to Preprocess, but check privilege need table
	// information, which is collected into visitInfo during logical plan builder.
	if pm := privilege.GetPrivilegeManager(ctx); pm != nil {
		if !checkPrivilege(pm, builder.visitInfo) {
			return nil, errors.New("privilege check fail")
		}
	}

	// code_analysis 优化主要是为select准备的
	if logic, ok := p.(LogicalPlan); ok {
		return doOptimize(builder.optFlag, logic)
	} else {
		logrus.Infof("insert will not trigger optimize, not a logical plan")
	}
	if execPlan, ok := p.(*Execute); ok {
		err := execPlan.optimizePreparedPlan(ctx, is)
		return p, errors.Trace(err)
	} else {
		logrus.Infof("Insert is not a Execute")
	}

	return p, nil
}

// BuildLogicalPlan used to build logical plan from ast.Node.
func BuildLogicalPlan(ctx sessionctx.Context, node ast.Node, is infoschema.InfoSchema) (Plan, error) {
	ctx.GetSessionVars().PlanID = 0
	builder := &planBuilder{
		ctx:       ctx,
		is:        is,
		colMapper: make(map[*ast.ColumnNameExpr]int),
	}
	p := builder.build(node)
	if builder.err != nil {
		return nil, errors.Trace(builder.err)
	}
	return p, nil
}

func checkPrivilege(pm privilege.Manager, vs []visitInfo) bool {
	logrus.Infof("check privilege by visitinfo: %s", vs)
	for _, v := range vs {
		if !pm.RequestVerification(v.db, v.table, v.column, v.privilege) {
			return false
		}
	}
	return true
}

func doOptimize(flag uint64, logic LogicalPlan) (PhysicalPlan, error) {
	logic, err := logicalOptimize(flag, logic)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !AllowCartesianProduct && existsCartesianProduct(logic) {
		return nil, errors.Trace(ErrCartesianProductUnsupported)
	}
	physical, err := physicalOptimize(logic)
	if err != nil {
		return nil, errors.Trace(err)
	}
	finalPlan := eliminatePhysicalProjection(physical)
	return finalPlan, nil
}

func logicalOptimize(flag uint64, logic LogicalPlan) (LogicalPlan, error) {
	var err error
	for i, rule := range optRuleList {
		// The order of flags is same as the order of optRule in the list.
		// We use a bitmask to record which opt rules should be used. If the i-th bit is 1, it means we should
		// apply i-th optimizing rule.
		if flag&(1<<uint(i)) == 0 {
			continue
		}
		logic, err = rule.optimize(logic)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return logic, errors.Trace(err)
}

func physicalOptimize(logic LogicalPlan) (PhysicalPlan, error) {
	logic.preparePossibleProperties()
	_, err := logic.deriveStats()
	if err != nil {
		return nil, errors.Trace(err)
	}
	t, err := logic.findBestTask(&requiredProp{taskTp: rootTaskType, expectedCnt: math.MaxFloat64})
	if err != nil {
		return nil, errors.Trace(err)
	}
	if t.invalid() {
		return nil, ErrInternal.GenByArgs("Can't find a proper physical plan for this query")
	}
	p := t.plan()
	p.ResolveIndices()
	return p, nil
}

func existsCartesianProduct(p LogicalPlan) bool {
	if join, ok := p.(*LogicalJoin); ok && len(join.EqualConditions) == 0 {
		return join.JoinType == InnerJoin || join.JoinType == LeftOuterJoin || join.JoinType == RightOuterJoin
	}
	for _, child := range p.Children() {
		if existsCartesianProduct(child) {
			return true
		}
	}
	return false
}

func init() {
	expression.EvalAstExpr = evalAstExpr
}
