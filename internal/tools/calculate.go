package tools

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type CalculateArgs struct {
	Expression string `json:"expression" desc:"A math expression e.g. '50000 / 8', 'sqrt(2500)', '(95000 + 67200) / 2'"`
}

type CalculateResult struct {
	Expression string `json:"expression"`
	Result     any    `json:"result,omitempty"`
	Formatted  string `json:"formatted,omitempty"`
	Error      string `json:"error,omitempty"`
}

func NewCalculateTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "calculate",
		Description: "Evaluate a mathematical expression.",
	}, calculateHandler)
}

func calculateHandler(_ tool.Context, args CalculateArgs) (CalculateResult, error) {
	result, err := evalExpr(args.Expression)
	if err != nil {
		return CalculateResult{
			Expression: args.Expression,
			Error:      err.Error(),
		}, nil
	}

	formatted := formatNumber(result)
	return CalculateResult{
		Expression: args.Expression,
		Result:     result,
		Formatted:  formatted,
	}, nil
}

func formatNumber(v float64) string {
	if v == math.Trunc(v) && !math.IsInf(v, 0) && !math.IsNaN(v) {
		return fmt.Sprintf("%s", addCommas(fmt.Sprintf("%.0f", v)))
	}
	return fmt.Sprintf("%s", addCommas(fmt.Sprintf("%.2f", v)))
}

func addCommas(s string) string {
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	negative := false
	if strings.HasPrefix(intPart, "-") {
		negative = true
		intPart = intPart[1:]
	}

	var result []byte
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}

	s = string(result)
	if negative {
		s = "-" + s
	}
	if len(parts) == 2 {
		s += "." + parts[1]
	}
	return s
}

// evalExpr evaluates a simple arithmetic expression using Go's parser.
func evalExpr(expr string) (float64, error) {
	// Handle common math function names by replacing them
	expr = strings.ReplaceAll(expr, "sqrt", "Sqrt")
	expr = strings.ReplaceAll(expr, "abs", "Abs")
	expr = strings.ReplaceAll(expr, "pow", "Pow")

	// Try to parse and evaluate as a Go expression
	node, err := parser.ParseExpr(expr)
	if err != nil {
		return 0, fmt.Errorf("cannot parse expression: %s", expr)
	}
	return evalAST(node)
}

func evalAST(node ast.Expr) (float64, error) {
	switch n := node.(type) {
	case *ast.BasicLit:
		if n.Kind == token.INT || n.Kind == token.FLOAT {
			return strconv.ParseFloat(n.Value, 64)
		}
		return 0, fmt.Errorf("unsupported literal: %s", n.Value)

	case *ast.BinaryExpr:
		left, err := evalAST(n.X)
		if err != nil {
			return 0, err
		}
		right, err := evalAST(n.Y)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		case token.MUL:
			return left * right, nil
		case token.QUO:
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			return left / right, nil
		case token.REM:
			return math.Mod(left, right), nil
		default:
			return 0, fmt.Errorf("unsupported operator: %s", n.Op)
		}

	case *ast.ParenExpr:
		return evalAST(n.X)

	case *ast.UnaryExpr:
		val, err := evalAST(n.X)
		if err != nil {
			return 0, err
		}
		if n.Op == token.SUB {
			return -val, nil
		}
		return val, nil

	case *ast.CallExpr:
		fn, ok := n.Fun.(*ast.Ident)
		if !ok {
			return 0, fmt.Errorf("unsupported function call")
		}
		var fnArgs []float64
		for _, arg := range n.Args {
			v, err := evalAST(arg)
			if err != nil {
				return 0, err
			}
			fnArgs = append(fnArgs, v)
		}
		switch fn.Name {
		case "Sqrt":
			if len(fnArgs) != 1 {
				return 0, fmt.Errorf("sqrt requires 1 argument")
			}
			return math.Sqrt(fnArgs[0]), nil
		case "Abs":
			if len(fnArgs) != 1 {
				return 0, fmt.Errorf("abs requires 1 argument")
			}
			return math.Abs(fnArgs[0]), nil
		case "Pow":
			if len(fnArgs) != 2 {
				return 0, fmt.Errorf("pow requires 2 arguments")
			}
			return math.Pow(fnArgs[0], fnArgs[1]), nil
		default:
			return 0, fmt.Errorf("unsupported function: %s", fn.Name)
		}

	case *ast.Ident:
		switch n.Name {
		case "pi", "Pi":
			return math.Pi, nil
		case "e", "E":
			return math.E, nil
		default:
			return 0, fmt.Errorf("unknown identifier: %s", n.Name)
		}

	default:
		return 0, fmt.Errorf("unsupported expression type: %T", node)
	}
}
