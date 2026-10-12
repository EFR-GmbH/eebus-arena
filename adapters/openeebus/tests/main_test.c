#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <errno.h>
#include <signal.h>
#include <sys/wait.h>

static unsigned signals_sent;
static unsigned waits;
static pid_t signaled_pid;

static int test_kill(pid_t pid, int signal_number) {
  assert(signal_number == SIGTERM);
  signaled_pid = pid;
  signals_sent++;
  return 0;
}

static pid_t test_waitpid(pid_t pid, int* status, int options) {
  (void)status;
  assert(options == 0);
  waits++;
  if (waits == 1) {
    errno = EINTR;
    return -1;
  }
  return pid;
}

// Exercise the real cleanup helper while ensuring the test cannot signal a
// process. Unused native adapter functions are discarded at link time.
#define kill test_kill
#define waitpid test_waitpid
#define main arena_adapter_main
#include "../main.c"
#undef main
#undef waitpid
#undef kill

int main(void) {
  stop_child(-1);
  stop_child(0);
  assert(signals_sent == 0);
  assert(waits == 0);

  stop_child(1234);
  assert(signals_sent == 1);
  assert(signaled_pid == 1234);
  assert(waits == 2);
  return 0;
}
