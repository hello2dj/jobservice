// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package env

import (
	"context"
	"sync"

	"github.com/hello2dj/jobservice/src/job"
)

// Context keep some sharable materials and system controlling channels.
// The system context.Context interface is also included.
type Context struct {
	// The system context with cancel capability.
	SystemContext context.Context

	// Coordination signal
	WG *sync.WaitGroup

	// Report errors to bootstrap component
	// Once error is reported by lower components, the whole system should exit
	ErrorChan chan error

	// The base job context reference
	// It will be the parent context of job execution context
	JobContext job.Context
}
