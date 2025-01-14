package watcher_test

import (
	"errors"
	"os"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/bbs/events"
	"code.cloudfoundry.org/bbs/events/eventfakes"
	"code.cloudfoundry.org/bbs/fake_bbs"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/bbs/models/test/model_helpers"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"code.cloudfoundry.org/runtimeschema/cc_messages"
	"code.cloudfoundry.org/tps/cc_client/fakes"
	"code.cloudfoundry.org/tps/watcher"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
)

type EventHolder struct {
	event models.Event
}

var nilEventHolder = EventHolder{}

var _ = Describe("Watcher", func() {
	var (
		eventSource   *eventfakes.FakeEventSource
		bbsClient     *fake_bbs.FakeInternalClient
		ccClient      *fakes.FakeCcClient
		watcherRunner *watcher.Watcher
		process       ifrit.Process

		logger *lagertest.TestLogger

		nextErr   atomic.Value
		nextEvent atomic.Value
	)

	BeforeEach(func() {
		eventSource = new(eventfakes.FakeEventSource)
		bbsClient = new(fake_bbs.FakeInternalClient)
		bbsClient.SubscribeToInstanceEventsReturns(eventSource, nil)

		logger = lagertest.NewTestLogger("test")
		ccClient = new(fakes.FakeCcClient)

		var err error
		watcherRunner, err = watcher.NewWatcher(logger, 500, 10*time.Millisecond, bbsClient, ccClient)
		Expect(err).NotTo(HaveOccurred())

		nextErr = atomic.Value{}
		nextErr := nextErr
		nextEvent.Store(nilEventHolder)

		eventSource.CloseStub = func() error {
			nextErr.Store(errors.New("closed"))
			return nil
		}

		eventSource.NextStub = func() (models.Event, error) {
			time.Sleep(10 * time.Millisecond)
			if eventHolder := nextEvent.Load(); eventHolder != nilEventHolder {
				nextEvent.Store(nilEventHolder)

				eh := eventHolder.(EventHolder)
				if eh.event != nil {
					return eh.event, nil
				}
			}

			if err := nextErr.Load(); err != nil {
				return nil, err.(error)
			}

			return nil, nil
		}
	})

	JustBeforeEach(func() {
		process = ifrit.Invoke(watcherRunner)
	})

	AfterEach(func() {
		process.Signal(os.Interrupt)
		Eventually(process.Wait()).Should(Receive())
	})

	Describe("Actual LRP crashes", func() {
		var actual *models.ActualLRP

		BeforeEach(func() {
			actual = makeCrashingActualLRP("process-guid", "instance-guid", 1, 3, 1, cc_messages.AppLRPDomain, "out of memory")
		})

		JustBeforeEach(func() {
			nextEvent.Store(EventHolder{models.NewActualLRPCrashedEvent(actual, actual)})
		})

		Context("and the application has the cc-app Domain", func() {
			It("calls AppCrashed", func() {
				Eventually(ccClient.AppCrashedCallCount).Should(Equal(1))
				guid, crashed, _ := ccClient.AppCrashedArgsForCall(0)
				Expect(guid).To(Equal("process-guid"))
				Expect(crashed).To(Equal(cc_messages.AppCrashedRequest{
					Instance:        "instance-guid",
					Index:           1,
					CellID:          "some-cell",
					Reason:          "CRASHED",
					ExitDescription: "out of memory",
					CrashCount:      1,
					CrashTimestamp:  3,
				}))

				Expect(logger).To(Say("app-crashed"))
			})
		})

		Context("and the application does not have the cc-app Domain", func() {
			var otherActual *models.ActualLRP

			BeforeEach(func() {
				otherActual = makeCrashingActualLRP("other-process-guid", "instance-guid", 1, 3, 1, "", "")

				event := EventHolder{models.NewActualLRPCrashedEvent(actual, actual)}
				otherEvent := EventHolder{models.NewActualLRPCrashedEvent(otherActual, otherActual)}
				events := []EventHolder{otherEvent, event}

				eventSource.NextStub = func() (models.Event, error) {
					var e EventHolder
					time.Sleep(10 * time.Millisecond)
					if len(events) == 0 {
						return nil, nil
					}
					e, events = events[0], events[1:]
					return e.event, nil
				}
			})

			It("does not call AppCrashed", func() {
				Eventually(ccClient.AppCrashedCallCount).Should(Equal(1))
				buffer := logger.Buffer()
				Expect(buffer).To(Say("process-guid"))
				Expect(buffer).NotTo(Say("other-process-guid"))
			})
		})
	})

	Describe("Actual LRP instance removed", func() {
		var firstEventDomain string
		var firstEventPresence models.ActualLRP_Presence

		Context("if the application only has the cc-app Domain", func() {
			BeforeEach(func() {
				firstEventDomain = cc_messages.AppLRPDomain
				firstEventPresence = models.ActualLRP_Ordinary
				firstActual := makeRemovingActualLRP("first-process-guid", "first-instance-guid", 1, firstEventDomain, firstEventPresence)
				secondActual := makeRemovingActualLRP("other-process-guid", "other-instance-guid", 1, cc_messages.AppLRPDomain, models.ActualLRP_Evacuating)

				events := []EventHolder{
					{models.NewActualLRPInstanceRemovedEvent(firstActual)},
					{models.NewActualLRPInstanceRemovedEvent(secondActual)},
				}

				eventSource.NextStub = func() (models.Event, error) {
					var e EventHolder
					time.Sleep(10 * time.Millisecond)
					if len(events) == 0 {
						return nil, nil
					}
					e, events = events[0], events[1:]
					return e.event, nil
				}
			})

			It("does not call AppRescheduling for that event", func() {
				Eventually(ccClient.AppReschedulingCallCount).Should(Equal(1))
				buffer := logger.Buffer()
				Expect(buffer).NotTo(Say("first-process-guid"))
				Expect(buffer).To(Say("other-process-guid"))
			})
		})

		Context("if the application only has the Evacuating Presence", func() {
			BeforeEach(func() {
				firstEventDomain = cc_messages.RunningTaskDomain
				firstEventPresence = models.ActualLRP_Evacuating
				firstActual := makeRemovingActualLRP("first-process-guid", "first-instance-guid", 1, firstEventDomain, firstEventPresence)
				secondActual := makeRemovingActualLRP("other-process-guid", "other-instance-guid", 1, cc_messages.AppLRPDomain, models.ActualLRP_Evacuating)

				events := []EventHolder{
					{models.NewActualLRPInstanceRemovedEvent(firstActual)},
					{models.NewActualLRPInstanceRemovedEvent(secondActual)},
				}

				eventSource.NextStub = func() (models.Event, error) {
					var e EventHolder
					time.Sleep(10 * time.Millisecond)
					if len(events) == 0 {
						return nil, nil
					}
					e, events = events[0], events[1:]
					return e.event, nil
				}
			})

			It("does not call AppRescheduling for that event", func() {
				Eventually(ccClient.AppReschedulingCallCount).Should(Equal(1))
				buffer := logger.Buffer()
				Expect(buffer).NotTo(Say("first-process-guid"))
				Expect(buffer).To(Say("other-process-guid"))
			})
		})

		Context("if the application has both the cc-app Domain and the Evacuating Presence", func() {
			BeforeEach(func() {
				firstEventDomain = cc_messages.AppLRPDomain
				firstEventPresence = models.ActualLRP_Evacuating
				firstActual := makeRemovingActualLRP("first-process-guid", "first-instance-guid", 1, firstEventDomain, firstEventPresence)
				secondActual := makeRemovingActualLRP("other-process-guid", "other-instance-guid", 1, cc_messages.AppLRPDomain, models.ActualLRP_Evacuating)

				events := []EventHolder{
					{models.NewActualLRPInstanceRemovedEvent(firstActual)},
					{models.NewActualLRPInstanceRemovedEvent(secondActual)},
				}

				eventSource.NextStub = func() (models.Event, error) {
					var e EventHolder
					time.Sleep(10 * time.Millisecond)
					if len(events) == 0 {
						return nil, nil
					}
					e, events = events[0], events[1:]
					return e.event, nil
				}
			})

			It("calls AppRescheduling", func() {
				Eventually(ccClient.AppReschedulingCallCount).Should(Equal(1))
				guid, crashed, _ := ccClient.AppReschedulingArgsForCall(0)
				Expect(guid).To(Equal("first-process-guid"))
				Expect(crashed).To(Equal(cc_messages.AppReschedulingRequest{
					Instance: "first-instance-guid",
					Index:    1,
					CellID:   "some-cell",
					Reason:   "Cell is being evacuated",
				}))

				Expect(logger).To(Say("app-evacuating"))
			})
		})
	})

	Describe("Unrecognized events", func() {
		Context("when its not ActualLRPCrashed event", func() {
			BeforeEach(func() {
				nextEvent.Store(EventHolder{&models.ActualLRPCreatedEvent{}})
			})

			It("does not emit any more messages", func() {
				Consistently(ccClient.AppCrashedCallCount).Should(Equal(0))
			})
		})
	})

	Context("when the event source returns an error on subscribe", func() {
		var subscribeErr error

		BeforeEach(func() {
			subscribeErr = models.ErrUnknownError

			bbsClient.SubscribeToInstanceEventsStub = func(logger lager.Logger) (events.EventSource, error) {
				if bbsClient.SubscribeToInstanceEventsCallCount() > 1 {
					return eventSource, nil
				}
				return nil, subscribeErr
			}

			eventSource.NextStub = func() (models.Event, error) {
				return nil, errors.New("next-error")
			}
		})

		It("re-subscribes", func() {
			Eventually(bbsClient.SubscribeToInstanceEventsCallCount, 2*time.Second).Should(BeNumerically(">", 1))
		})

		Context("when re-subscribing fails", func() {
			It("retries", func() {
				Consistently(process.Wait()).ShouldNot(Receive())
			})
		})
	})

	Context("when the event source returns an error on next", func() {
		BeforeEach(func() {
			eventSource.NextStub = func() (models.Event, error) {
				return nil, errors.New("next-error")
			}
		})

		It("retries 3 times and then re-subscribes", func() {
			Eventually(bbsClient.SubscribeToInstanceEventsCallCount, 5*time.Second).Should(BeNumerically(">", 1))
			Expect(eventSource.NextCallCount()).Should(BeNumerically(">=", 3))
		})
	})

})

func makeCrashingActualLRP(processGuid, instanceGuid string, index, since, crashCount int32, domain, reason string) *models.ActualLRP {
	lrp := model_helpers.NewValidActualLRP(processGuid, index)
	lrp.InstanceGuid = instanceGuid
	lrp.Since = int64(since)
	lrp.CrashCount = crashCount
	lrp.Domain = domain
	lrp.CrashReason = reason

	return lrp
}

func makeRemovingActualLRP(processGuid, instanceGuid string, index int32, domain string, presence models.ActualLRP_Presence) *models.ActualLRP {
	lrp := model_helpers.NewValidActualLRP(processGuid, index)
	lrp.InstanceGuid = instanceGuid
	lrp.ActualLRPKey.Domain = domain
	lrp.Presence = presence

	return lrp
}
