package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type departmentReaderStub struct {
	definition      *UserAttributeDefinition
	definitionError error
	values          []UserAttributeValue
	valuesError     error
	definitionCalls int
	valueCalls      int
}

type blockingDepartmentReader struct{}

func (blockingDepartmentReader) GetDefinitionByKey(
	ctx context.Context,
	_ string,
) (*UserAttributeDefinition, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingDepartmentReader) GetUserAttributes(
	context.Context,
	int64,
) ([]UserAttributeValue, error) {
	return nil, nil
}

func (s *departmentReaderStub) GetDefinitionByKey(
	context.Context,
	string,
) (*UserAttributeDefinition, error) {
	s.definitionCalls++
	return s.definition, s.definitionError
}

func (s *departmentReaderStub) GetUserAttributes(
	context.Context,
	int64,
) ([]UserAttributeValue, error) {
	s.valueCalls++
	return s.values, s.valuesError
}

func newDepartmentResolverForTest(
	reader departmentAttributeReader,
) *cachedDepartmentResolver {
	return newDepartmentResolver(
		reader,
		time.Minute,
		time.Second,
		time.Second,
	)
}

func TestDepartmentResolverCachesHit(t *testing.T) {
	reader := &departmentReaderStub{
		definition: &UserAttributeDefinition{ID: 7},
		values: []UserAttributeValue{{
			UserID:      42,
			AttributeID: 7,
			Value:       "finance",
		}},
	}
	resolver := newDepartmentResolverForTest(reader)

	require.Equal(t, "finance", resolver.Resolve(context.Background(), 42))
	require.Equal(t, "finance", resolver.Resolve(context.Background(), 42))
	require.Equal(t, 1, reader.definitionCalls)
	require.Equal(t, 1, reader.valueCalls)
}

func TestDepartmentResolverMissingValueReturnsUnknown(t *testing.T) {
	reader := &departmentReaderStub{
		definition: &UserAttributeDefinition{ID: 7},
	}
	resolver := newDepartmentResolverForTest(reader)

	require.Equal(
		t,
		UnknownDepartmentCode,
		resolver.Resolve(context.Background(), 42),
	)
}

func TestDepartmentResolverQueryErrorReturnsUnknown(t *testing.T) {
	reader := &departmentReaderStub{
		definitionError: errors.New("database unavailable"),
	}
	resolver := newDepartmentResolverForTest(reader)

	before := departmentErrorTotal.Load()
	code := resolver.Resolve(context.Background(), 42)

	require.Equal(t, UnknownDepartmentCode, code)
	require.Equal(t, before+1, departmentErrorTotal.Load())
}

func TestDepartmentResolverTimeoutReturnsUnknown(t *testing.T) {
	resolver := newDepartmentResolver(
		blockingDepartmentReader{},
		time.Minute,
		time.Second,
		10*time.Millisecond,
	)
	before := departmentErrorTotal.Load()
	started := time.Now()

	code := resolver.Resolve(context.Background(), 42)

	require.Equal(t, UnknownDepartmentCode, code)
	require.Less(t, time.Since(started), 200*time.Millisecond)
	require.Equal(t, before+1, departmentErrorTotal.Load())
}
