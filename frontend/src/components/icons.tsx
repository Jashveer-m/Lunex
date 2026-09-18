// A small stroke icon set (24-unit grid, 1.75 stroke) so the app carries no
// icon dependency. Each icon inherits currentColor and sizes with font-size.
import type { ReactNode, SVGProps } from 'react'

type IconProps = SVGProps<SVGSVGElement> & { size?: number }

function base(paths: ReactNode) {
  return function Icon({ size = 16, ...rest }: IconProps) {
    return (
      <svg
        width={size}
        height={size}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth={1.75}
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        {...rest}
      >
        {paths}
      </svg>
    )
  }
}

export const IconHome = base(
  <>
    <path d="M4 10.5 12 4l8 6.5V19a1 1 0 0 1-1 1h-4.5v-5.5h-5V20H5a1 1 0 0 1-1-1z" />
  </>,
)
export const IconFile = base(
  <>
    <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
    <path d="M14 3v5h5M9 13h6M9 17h4" />
  </>,
)
export const IconChat = base(<path d="M20 12a8 8 0 0 1-11.6 7.1L4 20l1-4.1A8 8 0 1 1 20 12z" />)
export const IconSpark = base(
  <>
    <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1" />
    <circle cx="12" cy="12" r="2.5" />
  </>,
)
export const IconInbox = base(
  <>
    <path d="M4 13.5 6.5 5h11l2.5 8.5V19a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1z" />
    <path d="M4 13.5h4.5l1 2h5l1-2H20" />
  </>,
)
export const IconGraph = base(
  <>
    <circle cx="6" cy="6" r="2.2" />
    <circle cx="18" cy="8" r="2.2" />
    <circle cx="9" cy="18" r="2.2" />
    <path d="M8 7l7.9 0.8M7 8l1.5 7.9M16.6 9.8 10.6 16.4" />
  </>,
)
export const IconPlus = base(<path d="M12 5v14M5 12h14" />)
export const IconX = base(<path d="M6 6l12 12M18 6 6 18" />)
export const IconCheck = base(<path d="m5 12.5 4.5 4.5L19 7.5" />)
export const IconTrash = base(
  <>
    <path d="M4 7h16M9 7V4.5h6V7M6.5 7l1 12.5a1 1 0 0 0 1 .9h7a1 1 0 0 0 1-.9l1-12.5" />
  </>,
)
export const IconPencil = base(<path d="M4 20h4L19 9a2.8 2.8 0 0 0-4-4L4 16z" />)
export const IconUpload = base(
  <>
    <path d="M12 15V4M7.5 8.5 12 4l4.5 4.5" />
    <path d="M4 15v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3" />
  </>,
)
export const IconSend = base(<path d="M5 12h13M12.5 6 18.5 12l-6 6" />)
export const IconStop = base(<rect x="7" y="7" width="10" height="10" rx="1.5" />)
export const IconLogout = base(
  <>
    <path d="M14 4h4a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-4" />
    <path d="M10 16l-4-4 4-4M6 12h10" />
  </>,
)
export const IconRefresh = base(
  <>
    <path d="M20 11a8 8 0 0 0-14.6-4.5L4 8" />
    <path d="M4 4v4h4M4 13a8 8 0 0 0 14.6 4.5L20 16" />
    <path d="M20 20v-4h-4" />
  </>,
)
export const IconChevron = base(<path d="m9 6 6 6-6 6" />)
export const IconSearch = base(
  <>
    <circle cx="11" cy="11" r="6.5" />
    <path d="m20 20-4.2-4.2" />
  </>,
)
export const IconAlert = base(
  <>
    <circle cx="12" cy="12" r="8.5" />
    <path d="M12 7.5v5.5M12 16.2v.3" />
  </>,
)
export const IconShield = base(<path d="M12 3.5 5 6v5.5c0 4.2 2.9 7.7 7 9 4.1-1.3 7-4.8 7-9V6z" />)
export const IconFlag = base(<path d="M5 21V4h11l-1.5 4L16 12H5" />)
export const IconNote = base(
  <>
    <path d="M5 4h14v11l-5 5H5z" />
    <path d="M14 20v-5h5M8.5 9h7M8.5 12.5h4" />
  </>,
)
export const IconBrain = base(
  <>
    <path d="M9.5 4.5a3 3 0 0 0-3 3 3 3 0 0 0-2 5.2A3 3 0 0 0 7 17.5a2.5 2.5 0 0 0 5 .5V5.5a2 2 0 0 0-2.5-1z" />
    <path d="M14.5 4.5a3 3 0 0 1 3 3 3 3 0 0 1 2 5.2 3 3 0 0 1-2.5 4.8 2.5 2.5 0 0 1-5 .5" />
  </>,
)
export const IconCalendar = base(
  <>
    <rect x="4" y="5.5" width="16" height="14.5" rx="2" />
    <path d="M4 10h16M8.5 3.5v4M15.5 3.5v4" />
  </>,
)
export const IconPin = base(
  <>
    <path d="M12 21s6.5-5.6 6.5-11a6.5 6.5 0 0 0-13 0c0 5.4 6.5 11 6.5 11z" />
    <circle cx="12" cy="10" r="2.3" />
  </>,
)
export const IconWallet = base(
  <>
    <path d="M4 7.5v10A2.5 2.5 0 0 0 6.5 20H18a2 2 0 0 0 2-2v-8a2 2 0 0 0-2-2H6.5A2.5 2.5 0 0 1 4 7.5 2.5 2.5 0 0 1 6.5 5H17" />
    <circle cx="16" cy="14" r="1.1" />
  </>,
)
