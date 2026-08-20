package repository_test

import (
	"github.com/aphronio/dorf/internal/repository"
	"github.com/aphronio/dorf/internal/spine"
)

var _ repository.Execution = spine.RepositoryService{}
